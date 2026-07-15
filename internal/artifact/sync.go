package artifact

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"maps"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/klauspost/compress/zstd"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
)

const (
	formatVersion     = 1
	originStateKey    = "artifact_origin_id"
	importStatePrefix = "artifact_import:"
	exportStatePrefix = "artifact_export:"
	tempFilePrefix    = ".tmp-"
	manifestExtension = ".json.zst"
	segmentExtension  = ".ndjson.zst"

	manifestDecodedLimit = int64(16 << 20)
	segmentDecodedLimit  = int64(64 << 20)
	segmentTargetSize    = int64(32 << 20)
	// zstd.NewWriter documents an 8 MiB maximum default window. Matching it
	// keeps existing package-written artifacts readable without accepting
	// attacker-selected large decoder windows.
	zstdMaxWindowSize = uint64(8 << 20)

	// Cardinality caps complement the byte caps: 4,096 records keeps one
	// segment's decoded object graph bounded, while 32,768 records and 256 MiB
	// leave ample room for unusually long sessions without letting many valid
	// chunks amplify during aggregation. Sixteen references accommodate uneven
	// 32 MiB chunks; the aggregate byte cap remains the final session bound.
	maxManifestSegments    = 16
	maxManifestUsageEvents = 32_768
	maxSegmentMessages     = 4_096
	maxSessionMessages     = 32_768
	maxSessionDecodedBytes = int64(256 << 20)

	// Nested collections need independent caps because compact empty objects can
	// amplify far beyond the decoded byte budget when unmarshaled. A message may
	// still describe unusually wide tool fan-out, and one tool may retain a long
	// result history. Segment totals keep one decoded chunk modest; session totals
	// allow eight full nested-budget segments, matching the message-count ratio.
	maxMessageToolCalls    = 256
	maxToolResultEvents    = 1_024
	maxSegmentToolCalls    = 8_192
	maxSegmentResultEvents = 32_768
	maxSessionToolCalls    = 65_536
	maxSessionResultEvents = 262_144
)

// artifactLimits bounds decoded collection cardinality in addition to raw
// bytes. The production values are intentionally generous for real sessions
// while preventing small JSON records from amplifying into unbounded Go
// object graphs.
type artifactLimits struct {
	manifestSegments    int
	manifestUsageEvents int
	segmentMessages     int
	sessionMessages     int
	sessionDecodedBytes int64
	messageToolCalls    int
	toolResultEvents    int
	segmentToolCalls    int
	segmentResultEvents int
	sessionToolCalls    int
	sessionResultEvents int
}

func productionArtifactLimits() artifactLimits {
	return artifactLimits{
		manifestSegments:    maxManifestSegments,
		manifestUsageEvents: maxManifestUsageEvents,
		segmentMessages:     maxSegmentMessages,
		sessionMessages:     maxSessionMessages,
		sessionDecodedBytes: maxSessionDecodedBytes,
		messageToolCalls:    maxMessageToolCalls,
		toolResultEvents:    maxToolResultEvents,
		segmentToolCalls:    maxSegmentToolCalls,
		segmentResultEvents: maxSegmentResultEvents,
		sessionToolCalls:    maxSessionToolCalls,
		sessionResultEvents: maxSessionResultEvents,
	}
}

type nestedCollectionCounts struct {
	toolCalls    int
	resultEvents int
}

type segmentPreflight struct {
	records [][]byte
	nested  nestedCollectionCounts
}

func exceedsCollectionLimit(current, additional, limit int) bool {
	return current > limit || additional > limit-current
}

var errIncompleteArtifact = errors.New("incomplete artifact")

// errCorruptArtifact reports an artifact whose bytes fail content validation:
// a hash mismatch or an undecodable compressed stream. Corrupt artifacts are
// quarantined and skipped so one bad file cannot abort sync permanently.
var errCorruptArtifact = errors.New("corrupt artifact")

// quarantineSuffix is appended to a corrupt artifact's filename when it is
// quarantined. Quarantined files are ignored by every read path and are never
// mirrored by CopyUnion, so a re-fetched valid copy can take the original name.
const quarantineSuffix = ".corrupt"

var errFutureArtifactVersion = errors.New("future artifact version")

var writeFileAtomicBeforeCommit func(path string)
var writeFileAtomicLink = os.Link

// SyncOptions configures a local-first artifact folder sync.
type SyncOptions struct {
	DataDir string
	Target  string
	Origin  string
	// Now is the wall-clock source for advancing the metadata HLC past
	// observed remote events. When nil, time.Now is used. Sharing it with the
	// local metadata recorder keeps import and local edits on one time base.
	Now func() time.Time
	// Token is the Bearer token for an HTTP peer target. It is ignored by
	// folder and object-store targets.
	Token string
	// AllowInsecure permits plaintext HTTP to a non-loopback peer. Loopback
	// HTTP remains allowed without this override.
	AllowInsecure bool
	// BaselineMetadata writes metadata events for existing local curation before
	// exchanging artifacts. It is intended for first-time initialization.
	BaselineMetadata bool
	// OnDataChanged is called after a foreign import writes local rows.
	OnDataChanged func()
}

// Sync runs one artifact sync, selecting the transport from the target shape:
// an http(s):// URL uses the HTTP peer transport, anything else is treated as a
// local folder target.
func Sync(ctx context.Context, database *db.DB, opts SyncOptions) (SyncResult, error) {
	if opts.Target == "" {
		return SyncResult{}, errors.New("artifact sync target is required")
	}
	if IsHTTPTarget(opts.Target) {
		tr, err := newHTTPTransport(opts.Target, opts.Token, opts.AllowInsecure)
		if err != nil {
			return SyncResult{}, err
		}
		return syncWithTransport(ctx, database, opts, tr)
	}
	if IsObjectTarget(opts.Target) {
		tr, err := newObjectTransport(opts.Target, ObjectStoreOptionsFromEnv())
		if err != nil {
			return SyncResult{}, err
		}
		return syncWithTransport(ctx, database, opts, tr)
	}
	return syncWithTransport(ctx, database, opts, &folderTransport{target: opts.Target})
}

// SyncResult summarizes a folder artifact sync run.
type SyncResult struct {
	Origin           string
	ExportedSessions int
	ImportedSessions int
	ImportedMessages int
	ImportedMetadata int
}

// ImportResult summarizes local rows changed by artifact import.
type ImportResult struct {
	Sessions int
	Messages int
	Metadata int
	Deferred int
}

// Changed reports whether the import wrote user-visible local data.
func (r ImportResult) Changed() bool {
	return r.Sessions > 0 || r.Messages > 0 || r.Metadata > 0
}

// SyncFolder exports local sessions to the local artifact store, exchanges the
// store with target, and imports foreign origins from the exchanged artifacts.
func SyncFolder(ctx context.Context, database *db.DB, opts SyncOptions) (SyncResult, error) {
	if opts.Target == "" {
		return SyncResult{}, errors.New("artifact sync target is required")
	}
	return syncWithTransport(ctx, database, opts, &folderTransport{target: opts.Target})
}

// syncWithTransport runs one artifact sync over any transport: export local
// sessions, exchange the store with the remote via set-union, then import
// foreign origins. Folder, HTTP peer, and object-store targets differ only in
// the transport's Prepare and Exchange.
func syncWithTransport(
	ctx context.Context,
	database *db.DB,
	opts SyncOptions,
	tr Transport,
) (SyncResult, error) {
	if opts.DataDir == "" {
		return SyncResult{}, errors.New("artifact sync data dir is required")
	}
	localRoot := filepath.Join(opts.DataDir, "artifacts")
	if err := tr.Prepare(ctx, localRoot); err != nil {
		return SyncResult{}, err
	}
	if err := os.MkdirAll(localRoot, 0o755); err != nil {
		return SyncResult{}, fmt.Errorf("creating local artifact store: %w", err)
	}
	origin := opts.Origin
	if origin == "" {
		var err error
		origin, err = EnsureOrigin(database)
		if err != nil {
			return SyncResult{}, err
		}
	} else if err := validateOriginID(origin); err != nil {
		return SyncResult{}, err
	}

	clock := NewHLCClock(database, HLCClockOptions{Now: opts.Now})
	var imported ImportResult
	var baselineSnapshot db.MetadataBaselineSnapshot
	if opts.BaselineMetadata {
		var err error
		baselineSnapshot, err = database.MetadataBaselineSnapshot(ctx)
		if err != nil {
			return SyncResult{}, err
		}
		if err := tr.Exchange(ctx, localRoot); err != nil {
			return SyncResult{}, err
		}
		preBaselineImported, err := importDetailed(ctx, database, clock, localRoot, origin)
		if err != nil {
			return SyncResult{}, err
		}
		imported.Sessions += preBaselineImported.Sessions
		imported.Messages += preBaselineImported.Messages
		imported.Metadata += preBaselineImported.Metadata

		recorder := NewMetadataRecorder(database, MetadataRecorderOptions{
			DataDir: opts.DataDir,
			Origin:  origin,
			Now:     opts.Now,
		})
		if _, err := recorder.AppendBaselineSnapshot(ctx, baselineSnapshot); err != nil {
			return SyncResult{}, err
		}
	}
	exported, err := Export(ctx, database, localRoot, origin)
	if err != nil {
		return SyncResult{}, err
	}
	if err := tr.Exchange(ctx, localRoot); err != nil {
		return SyncResult{}, err
	}
	postExportImported, err := importDetailed(ctx, database, clock, localRoot, origin)
	if err != nil {
		return SyncResult{}, err
	}
	imported.Sessions += postExportImported.Sessions
	imported.Messages += postExportImported.Messages
	imported.Metadata += postExportImported.Metadata
	if imported.Changed() && opts.OnDataChanged != nil {
		opts.OnDataChanged()
	}
	return SyncResult{
		Origin:           origin,
		ExportedSessions: exported,
		ImportedSessions: imported.Sessions,
		ImportedMessages: imported.Messages,
		ImportedMetadata: imported.Metadata,
	}, nil
}

// EnsureOrigin returns the persisted origin ID, creating one when absent.
func EnsureOrigin(database *db.DB) (string, error) {
	origin, err := StoredOrigin(database)
	if err != nil {
		return "", err
	}
	if origin != "" {
		return origin, nil
	}
	origin, err = newOriginID()
	if err != nil {
		return "", err
	}
	if err := validateOriginID(origin); err != nil {
		return "", fmt.Errorf("generated artifact origin: %w", err)
	}
	if err := database.SetSyncState(originStateKey, origin); err != nil {
		return "", fmt.Errorf("persisting artifact origin: %w", err)
	}
	return origin, nil
}

// AdoptOrigin persists origin as this machine's artifact origin in the database
// sync state so DB-derived lookups (EnsureOrigin and its callers) agree with the
// authoritative config origin. It validates the input and is idempotent: it only
// writes when the stored value differs. The config origin always wins, so a
// previously stored value is overwritten to converge on a single origin.
func AdoptOrigin(database *db.DB, origin string) error {
	if err := validateOriginID(origin); err != nil {
		return fmt.Errorf("adopting artifact origin: %w", err)
	}
	existing, err := StoredOrigin(database)
	if err != nil {
		return err
	}
	if existing == origin {
		return nil
	}
	if err := database.SetSyncState(originStateKey, origin); err != nil {
		return fmt.Errorf("persisting artifact origin: %w", err)
	}
	return nil
}

// StoredOrigin returns the persisted origin ID without creating one.
func StoredOrigin(database *db.DB) (string, error) {
	origin, err := database.GetSyncState(originStateKey)
	if err != nil {
		return "", fmt.Errorf("reading artifact origin: %w", err)
	}
	if origin != "" {
		if err := validateOriginID(origin); err != nil {
			return "", fmt.Errorf("stored artifact origin: %w", err)
		}
		return origin, nil
	}
	return "", nil
}

type syncStateValueReader interface {
	SyncStateValues(keys []string) (map[string]string, error)
}

// ImportedSessionIDs returns the candidate session IDs with durable artifact
// import provenance. A foreign machine~id shape is shared by other import
// mechanisms, so callers must query the exact provenance keys rather than
// infer artifact ownership from the session row or scan all historical imports.
func ImportedSessionIDs(
	database syncStateValueReader, candidateIDs []string,
) (map[string]struct{}, error) {
	ids := make(map[string]struct{})
	if len(candidateIDs) == 0 {
		return ids, nil
	}
	keys := make([]string, 0, len(candidateIDs))
	keyToID := make(map[string]string, len(candidateIDs))
	for _, gid := range candidateIDs {
		origin, nativeID, ok := strings.Cut(gid, "~")
		if !ok || origin == "" || nativeID == "" {
			continue
		}
		key := importStateKey(origin, gid)
		keys = append(keys, key)
		keyToID[key] = gid
	}
	if len(keys) == 0 {
		return ids, nil
	}
	states, err := database.SyncStateValues(keys)
	if err != nil {
		return nil, fmt.Errorf("reading artifact import provenance: %w", err)
	}
	for key := range states {
		if gid, ok := keyToID[key]; ok {
			ids[gid] = struct{}{}
		}
	}
	return ids, nil
}

// CountImportedCheckpointSessions returns how many sessions in one checkpoint
// have landed at the exact manifest version it publishes. Import provenance is
// independent of the session row's current lifecycle state, so a locally
// trashed session remains landed while a stale active row does not.
func CountImportedCheckpointSessions(
	database syncStateValueReader, origin string, sessionManifests map[string]string,
) (int, error) {
	if len(sessionManifests) == 0 {
		return 0, nil
	}
	keys := make([]string, 0, len(sessionManifests))
	expectedByKey := make(map[string]string, len(sessionManifests))
	for gid, manifestHash := range sessionManifests {
		key := importStateKey(origin, gid)
		keys = append(keys, key)
		expectedByKey[key] = manifestHash
	}
	states, err := database.SyncStateValues(keys)
	if err != nil {
		return 0, fmt.Errorf("reading checkpoint import provenance: %w", err)
	}
	landed := 0
	for key, expectedHash := range expectedByKey {
		if states[key] == expectedHash {
			landed++
		}
	}
	return landed, nil
}

func newOriginID() (string, error) {
	host, err := os.Hostname()
	if err != nil || strings.TrimSpace(host) == "" {
		host = "machine"
	}
	host = sanitizeOriginPart(host)
	if host == "" || host == "local" {
		host = "machine"
	}
	var suffix [3]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", fmt.Errorf("generating artifact origin suffix: %w", err)
	}
	return fmt.Sprintf("%s-%s", host, hex.EncodeToString(suffix[:])), nil
}

func validateOriginID(origin string) error {
	return config.ValidateArtifactOriginID(origin)
}

func validateDisjointRoots(localRoot, target string) error {
	localAbs, err := filepath.Abs(localRoot)
	if err != nil {
		return fmt.Errorf("resolving local artifact store: %w", err)
	}
	targetAbs, err := filepath.Abs(target)
	if err != nil {
		return fmt.Errorf("resolving artifact sync target: %w", err)
	}
	localAbs = filepath.Clean(localAbs)
	targetAbs = filepath.Clean(targetAbs)
	localCanonical, err := canonicalArtifactPath(localAbs)
	if err != nil {
		return fmt.Errorf("resolving local artifact store symlinks: %w", err)
	}
	targetCanonical, err := canonicalArtifactPath(targetAbs)
	if err != nil {
		return fmt.Errorf("resolving artifact sync target symlinks: %w", err)
	}
	if rootsOverlap(localAbs, targetAbs) || rootsOverlap(localCanonical, targetCanonical) {
		return fmt.Errorf(
			"artifact sync target %s must not overlap local artifact store %s",
			targetCanonical, localCanonical,
		)
	}
	return nil
}

func canonicalArtifactPath(path string) (string, error) {
	missing := make([]string, 0, 2)
	current := path
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			for _, part := range slices.Backward(missing) {
				resolved = filepath.Join(resolved, part)
			}
			return filepath.Clean(resolved), nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", err
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

func rootsOverlap(a, b string) bool {
	return a == b || pathContains(a, b) || pathContains(b, a)
}

func pathContains(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func sanitizeOriginPart(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	lastDash := false
	for _, r := range s {
		ok := r >= 'a' && r <= 'z' || r >= '0' && r <= '9'
		if ok {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

type checkpoint struct {
	Version  int               `json:"v"`
	Origin   string            `json:"origin"`
	Sequence int               `json:"seq"`
	Sessions map[string]string `json:"sessions"`
}

type manifest struct {
	Version         int                  `json:"v"`
	Origin          string               `json:"origin"`
	NativeSessionID string               `json:"native_session_id"`
	Session         manifestSession      `json:"session"`
	SessionName     *string              `json:"session_name,omitempty"`
	Segments        []string             `json:"segments"`
	UsageEvents     []artifactUsageEvent `json:"usage_events,omitempty"`
	RawSource       *rawSourceRef        `json:"raw_source,omitempty"`
	DataVersion     int                  `json:"data_version"`
	Generation      int                  `json:"generation"`
	// Signal state persisted on the session row but absent from the wire
	// Session above, which mirrors only db.Session's JSON-visible fields.
	// Carried explicitly so an imported session keeps its tool-call, context,
	// and quality signal state instead of resetting to false/zero. Secret-scan
	// state is deliberately not carried: findings live outside the manifest,
	// so imported sessions are treated as unscanned (see rewriteForImport).
	SessionHasToolCalls   bool                    `json:"session_has_tool_calls,omitempty"`
	SessionHasContextData bool                    `json:"session_has_context_data,omitempty"`
	SessionQualitySignals *manifestQualitySignals `json:"session_quality_signals,omitempty"`
}

type artifactUsageEvent struct {
	MessageOrdinal           *int     `json:"message_ordinal,omitempty"`
	Source                   string   `json:"source"`
	Model                    string   `json:"model"`
	InputTokens              int      `json:"input_tokens,omitempty"`
	OutputTokens             int      `json:"output_tokens,omitempty"`
	CacheCreationInputTokens int      `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     int      `json:"cache_read_input_tokens,omitempty"`
	ReasoningTokens          int      `json:"reasoning_tokens,omitempty"`
	CostUSD                  *float64 `json:"cost_usd,omitempty"`
	CostStatus               string   `json:"cost_status,omitempty"`
	CostSource               string   `json:"cost_source,omitempty"`
	OccurredAt               string   `json:"occurred_at,omitempty"`
	DedupKey                 string   `json:"dedup_key,omitempty"`
}

type rawSourceRef struct {
	Hash      string `json:"hash"`
	Size      int64  `json:"size"`
	MediaType string `json:"media_type,omitempty"`
	Path      string `json:"path,omitempty"`
}

type metadataEvent struct {
	Version    int             `json:"v"`
	HLC        string          `json:"hlc"`
	Origin     string          `json:"origin"`
	SessionGID string          `json:"session_gid"`
	Op         string          `json:"op"`
	Value      json.RawMessage `json:"value,omitempty"`
	Pin        *MetadataPin    `json:"pin,omitempty"`
}

type segmentMessage struct {
	Version           int               `json:"v"`
	Ordinal           int               `json:"ordinal"`
	Role              string            `json:"role"`
	Content           string            `json:"content"`
	ThinkingText      string            `json:"thinking_text,omitempty"`
	Timestamp         string            `json:"timestamp,omitempty"`
	HasThinking       bool              `json:"has_thinking,omitempty"`
	HasToolUse        bool              `json:"has_tool_use,omitempty"`
	ContentLength     int               `json:"content_length,omitempty"`
	Model             string            `json:"model,omitempty"`
	TokenUsage        json.RawMessage   `json:"token_usage,omitempty"`
	ContextTokens     int               `json:"context_tokens,omitempty"`
	OutputTokens      int               `json:"output_tokens,omitempty"`
	HasContextTokens  bool              `json:"has_context_tokens,omitempty"`
	HasOutputTokens   bool              `json:"has_output_tokens,omitempty"`
	ClaudeMessageID   string            `json:"claude_message_id,omitempty"`
	ClaudeRequestID   string            `json:"claude_request_id,omitempty"`
	ToolCalls         []segmentToolCall `json:"tool_calls,omitempty"`
	IsSystem          bool              `json:"is_system,omitempty"`
	SourceType        string            `json:"source_type,omitempty"`
	SourceSubtype     string            `json:"source_subtype,omitempty"`
	SourceUUID        string            `json:"source_uuid,omitempty"`
	SourceParentUUID  string            `json:"source_parent_uuid,omitempty"`
	IsSidechain       bool              `json:"is_sidechain,omitempty"`
	IsCompactBoundary bool              `json:"is_compact_boundary,omitempty"`
}

type segmentToolCall struct {
	CallIndex           int                  `json:"call_index"`
	ToolName            string               `json:"tool_name"`
	Category            string               `json:"category,omitempty"`
	ToolUseID           string               `json:"tool_use_id,omitempty"`
	InputJSON           string               `json:"input_json,omitempty"`
	FilePath            string               `json:"file_path,omitempty"`
	SkillName           string               `json:"skill_name,omitempty"`
	ResultContentLength int                  `json:"result_content_length,omitempty"`
	ResultContent       string               `json:"result_content,omitempty"`
	SubagentSessionID   string               `json:"subagent_session_id,omitempty"`
	ResultEvents        []segmentResultEvent `json:"result_events,omitempty"`
}

type segmentResultEvent struct {
	ToolUseID         string `json:"tool_use_id,omitempty"`
	AgentID           string `json:"agent_id,omitempty"`
	SubagentSessionID string `json:"subagent_session_id,omitempty"`
	Source            string `json:"source"`
	Status            string `json:"status"`
	Content           string `json:"content"`
	ContentLength     int    `json:"content_length,omitempty"`
	Timestamp         string `json:"timestamp,omitempty"`
	EventIndex        int    `json:"event_index"`
}

// Export writes current machine-owned sessions into root/origin.
func Export(ctx context.Context, database *db.DB, root, origin string) (int, error) {
	if err := validateOriginID(origin); err != nil {
		return 0, err
	}
	originRoot := filepath.Join(root, origin)
	for _, dir := range []string{"checkpoints", "manifests", "segments", "meta", "raw"} {
		if err := os.MkdirAll(filepath.Join(originRoot, dir), 0o755); err != nil {
			return 0, fmt.Errorf("creating %s: %w", dir, err)
		}
	}

	// Enumerate owned sessions from a raw query rather than the sidebar list API
	// so usage-only (zero-message) sessions are still exported.
	ids, err := database.ListOwnedSessionIDsForExport(ctx)
	if err != nil {
		return 0, err
	}
	var exported int
	sessions := map[string]string{}
	for _, id := range ids {
		stateKey := exportStateKey(origin, id)
		prevHash, err := database.GetSyncState(stateKey)
		if err != nil {
			return 0, fmt.Errorf("reading export state for %s: %w", id, err)
		}
		hash, changed, err := exportSession(ctx, database, originRoot, origin, id, prevHash)
		if err != nil {
			return 0, err
		}
		sessions[origin+"~"+id] = hash
		if changed {
			if err := database.SetSyncState(stateKey, hash); err != nil {
				return 0, fmt.Errorf("writing export state for %s: %w", id, err)
			}
			exported++
		}
	}
	if latestCheckpointMatches(originRoot, origin, sessions) {
		return exported, nil
	}
	seq, err := nextCheckpointSequence(originRoot)
	if err != nil {
		return 0, err
	}
	cp := checkpoint{Version: formatVersion, Origin: origin, Sequence: seq, Sessions: sessions}
	data, err := canonicalJSON(cp)
	if err != nil {
		return 0, err
	}
	checkpointPath := filepath.Join(originRoot, "checkpoints", fmt.Sprintf("cp-%010d.json", seq))
	if err := writeFileAtomic(checkpointPath, data, 0o644); err != nil {
		return 0, fmt.Errorf("writing checkpoint: %w", err)
	}
	return exported, nil
}

func latestCheckpointMatches(originRoot, origin string, sessions map[string]string) bool {
	paths, err := filepath.Glob(filepath.Join(originRoot, "checkpoints", "cp-*.json"))
	if err != nil || len(paths) == 0 {
		return false
	}
	path := paths[len(paths)-1]
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var cp checkpoint
	if err := json.Unmarshal(data, &cp); err != nil {
		return false
	}
	if err := validateCheckpoint(&cp, origin); err != nil {
		return false
	}
	if err := validateCheckpointSequenceIdentity(cp, filepath.Base(path)); err != nil {
		return false
	}
	return maps.Equal(cp.Sessions, sessions)
}

// nextCheckpointSequence derives the next checkpoint sequence from the
// highest existing checkpoint filename, quarantined ones included, so a
// corrupt latest checkpoint cannot block export and a sequence number that
// may already have been published to peers is never reused for different
// content. A latest live checkpoint whose body no longer parses is
// quarantined so read paths fall back to the previous valid checkpoint and a
// valid peer copy can heal it.
func nextCheckpointSequence(originRoot string) (int, error) {
	dir := filepath.Join(originRoot, "checkpoints")
	live, err := filepath.Glob(filepath.Join(dir, "cp-*.json"))
	if err != nil {
		return 0, err
	}
	quarantined, err := filepath.Glob(filepath.Join(dir, "cp-*.json"+quarantineSuffix))
	if err != nil {
		return 0, err
	}
	sort.Strings(live)
	if len(live) > 0 {
		latest := live[len(live)-1]
		data, err := os.ReadFile(latest)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return 0, err
		}
		var cp checkpoint
		if err == nil {
			if uerr := json.Unmarshal(data, &cp); uerr != nil {
				log.Printf("artifact: skipping corrupt checkpoint %s: %v", latest, uerr)
				quarantineArtifact(latest)
			}
		}
	}
	maxSeq := 0
	for _, p := range append(live, quarantined...) {
		name := strings.TrimSuffix(filepath.Base(p), quarantineSuffix)
		var seq int
		if _, serr := fmt.Sscanf(name, "cp-%d.json", &seq); serr != nil {
			continue
		}
		if seq > maxSeq {
			maxSeq = seq
		}
	}
	return maxSeq + 1, nil
}

func exportStateKey(origin, sessionID string) string {
	return exportStatePrefix + origin + ":" + sessionID
}

func exportSession(
	ctx context.Context,
	database *db.DB,
	originRoot, origin, sessionID, prevManifestHash string,
) (string, bool, error) {
	return exportSessionWithLimits(
		ctx, database, originRoot, origin, sessionID, prevManifestHash,
		productionArtifactLimits(),
	)
}

func exportSessionWithLimits(
	ctx context.Context,
	database *db.DB,
	originRoot, origin, sessionID, prevManifestHash string,
	limits artifactLimits,
) (string, bool, error) {
	sess, err := database.GetSessionFull(ctx, sessionID)
	if err != nil {
		return "", false, fmt.Errorf("loading session %s for artifact export: %w", sessionID, err)
	}
	if sess == nil {
		return "", false, fmt.Errorf("session %s disappeared during artifact export", sessionID)
	}
	msgs, err := database.GetAllMessages(ctx, sessionID)
	if err != nil {
		return "", false, fmt.Errorf("loading messages for artifact export %s: %w", sessionID, err)
	}
	usageEvents, err := database.GetUsageEvents(ctx, sessionID)
	if err != nil {
		return "", false, fmt.Errorf("loading usage events for artifact export %s: %w", sessionID, err)
	}
	if len(msgs) > limits.sessionMessages {
		return "", false, fmt.Errorf(
			"session message limit exceeded for %s: got %d, limit %d",
			sessionID, len(msgs), limits.sessionMessages,
		)
	}
	if len(usageEvents) > limits.manifestUsageEvents {
		return "", false, fmt.Errorf(
			"manifest usage event limit exceeded for %s: got %d, limit %d",
			sessionID, len(usageEvents), limits.manifestUsageEvents,
		)
	}
	if err := validateExportNestedCollections(msgs, limits); err != nil {
		return "", false, fmt.Errorf("validating nested collections for %s: %w", sessionID, err)
	}

	wireMessages := canonicalMessages(msgs)
	segmentHashes := make([]string, 0, 1)
	seenSegmentHashes := make(map[string]struct{})
	var sessionDecodedBytes int64
	if err := forEachEncodedSegmentWithLimits(wireMessages, limits, func(data []byte) error {
		if len(segmentHashes) >= limits.manifestSegments {
			return fmt.Errorf(
				"manifest segment reference limit exceeded for %s: limit %d",
				sessionID, limits.manifestSegments,
			)
		}
		segmentBytes := int64(len(data))
		if segmentBytes > limits.sessionDecodedBytes-sessionDecodedBytes {
			return fmt.Errorf(
				"session decoded byte limit exceeded for %s: limit %d",
				sessionID, limits.sessionDecodedBytes,
			)
		}
		segmentHash := hashHex(data)
		if _, ok := seenSegmentHashes[segmentHash]; ok {
			return fmt.Errorf("generated duplicate segment reference %s", segmentHash)
		}
		seenSegmentHashes[segmentHash] = struct{}{}
		segmentHashes = append(segmentHashes, segmentHash)
		sessionDecodedBytes += segmentBytes
		return nil
	}); err != nil {
		return "", false, err
	}

	wireSession := manifestSessionFromDB(*sess)
	wireSession.Machine = origin
	normalizeManifestSessionLocalState(&wireSession)
	m := manifest{
		Version:               formatVersion,
		Origin:                origin,
		NativeSessionID:       sessionID,
		Session:               wireSession,
		SessionName:           sess.SessionName,
		Segments:              segmentHashes,
		UsageEvents:           canonicalUsageEvents(usageEvents),
		DataVersion:           sess.DataVersion,
		Generation:            1,
		SessionHasToolCalls:   sess.HasToolCalls,
		SessionHasContextData: sess.HasContextData,
		SessionQualitySignals: manifestQualitySignalsFromDB(sess.StoredQualitySignals()),
	}
	manifestData, err := canonicalJSON(m)
	if err != nil {
		return "", false, err
	}
	if int64(len(manifestData)) > manifestDecodedLimit {
		return "", false, fmt.Errorf(
			"generated manifest exceeds %d-byte readable limit: got %d bytes",
			manifestDecodedLimit, len(manifestData),
		)
	}
	manifestHash := hashHex(manifestData)
	if compatible, err := previousManifestMatchesAfterLocalStateNormalization(
		originRoot, origin, sessionID, prevManifestHash, manifestHash,
	); err != nil {
		return "", false, err
	} else if compatible {
		return prevManifestHash, false, nil
	}
	manifestPath := filepath.Join(originRoot, "manifests", manifestHash+manifestExtension)
	if prevManifestHash == manifestHash {
		_, valid, err := readValidExportArtifacts(
			originRoot, origin, sessionID, manifestHash,
		)
		if err != nil {
			return "", false, err
		}
		if valid {
			return manifestHash, false, nil
		}
	}
	segmentIndex := 0
	if err := forEachEncodedSegmentWithLimits(wireMessages, limits, func(data []byte) error {
		segmentHash := hashHex(data)
		if segmentIndex >= len(segmentHashes) || segmentHashes[segmentIndex] != segmentHash {
			return errors.New("message segment encoding changed between export passes")
		}
		segmentIndex++
		if err := healComputedExportSegment(originRoot, segmentHash); err != nil {
			return fmt.Errorf("validating computed segment: %w", err)
		}
		segmentPath := filepath.Join(originRoot, "segments", segmentHash+segmentExtension)
		if err := writeCompressed(segmentPath, data); err != nil {
			return fmt.Errorf("writing segment: %w", err)
		}
		return nil
	}); err != nil {
		return "", false, fmt.Errorf("exporting segments for %s: %w", sessionID, err)
	}
	if err := writeCompressed(manifestPath, manifestData); err != nil {
		return "", false, fmt.Errorf("writing manifest for %s: %w", sessionID, err)
	}
	return manifestHash, true, nil
}

// healComputedExportSegment validates only the segment path derived from the
// current local messages. Corrupt bytes are quarantined by the shared reader so
// the subsequent immutable write can recreate them. Missing artifacts are
// expected; semantic errors such as a future segment version remain untouched
// and surface to the caller.
func healComputedExportSegment(originRoot, segmentHash string) error {
	_, err := readManifestMessages(originRoot, manifest{
		Segments: []string{segmentHash},
	})
	if err == nil || errors.Is(err, errIncompleteArtifact) ||
		errors.Is(err, errCorruptArtifact) {
		return nil
	}
	return err
}

func normalizeManifestSessionLocalState(sess *manifestSession) {
	// Keep non-content, machine-local state out of the canonical manifest so a
	// source-only change to it does not alter the content hash and trigger a
	// re-import that clears the importer's local findings. secret_leak_count is
	// import-discarded secret state (see rewriteForImport); local_modified_at is
	// the local sync watermark, which import ignores (the importer stamps its
	// own) -- and a secret rescan bumps both even when no exported message
	// content changed. The file_* fields are source-file bookkeeping that
	// import clears (see clearImportedSessionSourceState); a touch, move, or
	// re-download of the source file changes them without changing any
	// exported content.
	sess.SecretLeakCount = 0
	sess.LocalModifiedAt = nil
	sess.FilePath = nil
	sess.FileSize = nil
	sess.FileMtime = nil
	sess.FileInode = nil
	sess.FileDevice = nil
	sess.FileHash = nil
}

func previousManifestMatchesAfterLocalStateNormalization(
	originRoot, origin, sessionID, prevManifestHash, currentManifestHash string,
) (bool, error) {
	if prevManifestHash == "" || prevManifestHash == currentManifestHash {
		return false, nil
	}
	prev, valid, err := readValidExportArtifacts(
		originRoot, origin, sessionID, prevManifestHash,
	)
	if err != nil {
		return false, err
	}
	if !valid {
		return false, nil
	}
	normalizeManifestSessionLocalState(&prev.Session)
	data, err := canonicalJSON(prev)
	if err != nil {
		return false, err
	}
	return hashHex(data) == currentManifestHash, nil
}

func readValidExportArtifacts(
	originRoot, origin, sessionID, manifestHash string,
) (manifest, bool, error) {
	m, err := readManifest(originRoot, manifestHash)
	if err != nil {
		if errors.Is(err, errIncompleteArtifact) || errors.Is(err, errCorruptArtifact) {
			return manifest{}, false, nil
		}
		return manifest{}, false, err
	}
	if err := validateManifest(m, origin, origin+"~"+sessionID); err != nil {
		if errors.Is(err, errFutureArtifactVersion) {
			return manifest{}, false, nil
		}
		quarantineArtifact(filepath.Join(
			originRoot, KindManifests, manifestHash+manifestExtension,
		))
		return manifest{}, false, nil
	}
	if _, err := readManifestMessages(originRoot, m); err != nil {
		if errors.Is(err, errIncompleteArtifact) ||
			errors.Is(err, errCorruptArtifact) ||
			errors.Is(err, errFutureArtifactVersion) {
			return manifest{}, false, nil
		}
		return manifest{}, false, err
	}
	return m, true, nil
}

// Import reads every foreign origin under root and imports referenced sessions.
func Import(ctx context.Context, database *db.DB, root, localOrigin string) (int, int, error) {
	res, err := ImportDetailed(ctx, database, root, localOrigin)
	return res.Sessions, res.Messages, err
}

// ImportDetailed reads every foreign origin under root and imports referenced
// sessions plus metadata events.
func ImportDetailed(ctx context.Context, database *db.DB, root, localOrigin string) (ImportResult, error) {
	return importDetailed(ctx, database, nil, root, localOrigin)
}

// importDetailed imports foreign origins, advancing clock past observed remote
// metadata HLCs. When clock is nil a default clock backed by database is used so
// the persisted metadata clock is still advanced.
func importDetailed(
	ctx context.Context,
	database *db.DB,
	clock *HLCClock,
	root, localOrigin string,
) (ImportResult, error) {
	if clock == nil {
		clock = NewHLCClock(database, HLCClockOptions{})
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return ImportResult{}, nil
		}
		return ImportResult{}, fmt.Errorf("reading artifact roots: %w", err)
	}
	appliedEvents, err := database.MetadataAppliedEventIdentities(ctx)
	if err != nil {
		return ImportResult{}, err
	}
	foreignOrigins := make([]string, 0, len(entries))
	for _, ent := range entries {
		if ent.IsDir() && ent.Name() != localOrigin {
			foreignOrigins = append(foreignOrigins, ent.Name())
		}
	}
	var res ImportResult
	for _, origin := range foreignOrigins {
		originRes, err := importOriginContent(
			ctx, database, filepath.Join(root, origin), origin,
		)
		if err != nil {
			return res, err
		}
		res.Sessions += originRes.Sessions
		res.Messages += originRes.Messages
		res.Deferred += originRes.Deferred
	}
	for _, origin := range foreignOrigins {
		metadata, err := replayMetadata(
			ctx, database, clock, filepath.Join(root, origin), origin,
			localOrigin, appliedEvents,
		)
		if err != nil {
			return res, err
		}
		res.Metadata += metadata
	}
	return res, nil
}

func importOriginContent(
	ctx context.Context,
	database *db.DB,
	originRoot, origin string,
) (ImportResult, error) {
	cp, err := readLatestCompatibleCheckpoint(originRoot, origin)
	if err != nil {
		return ImportResult{}, fmt.Errorf("reading checkpoint for %s: %w", origin, err)
	}
	var res ImportResult
	if cp != nil {
		keys := make([]string, 0, len(cp.Sessions))
		for k := range cp.Sessions {
			keys = append(keys, k)
		}
		sort.Strings(keys)

		for _, gid := range keys {
			if err := ctx.Err(); err != nil {
				return res, err
			}
			manifestHash := cp.Sessions[gid]
			stateKey := importStateKey(origin, gid)
			prevHash, err := database.GetSyncState(stateKey)
			if err != nil {
				return res, fmt.Errorf("reading import state for %s: %w", gid, err)
			}
			if prevHash == manifestHash {
				continue
			}
			m, err := readManifest(originRoot, manifestHash)
			if err != nil {
				if errors.Is(err, errIncompleteArtifact) {
					res.Deferred++
					continue
				}
				if errors.Is(err, errCorruptArtifact) {
					log.Printf("artifact: skipping session %s from %s: %v", gid, origin, err)
					res.Deferred++
					continue
				}
				return res, err
			}
			if err := validateManifest(m, origin, gid); err != nil {
				if errors.Is(err, errFutureArtifactVersion) {
					continue
				}
				return res, err
			}
			msgs, err := readManifestMessages(originRoot, m)
			if err != nil {
				if errors.Is(err, errIncompleteArtifact) || errors.Is(err, errFutureArtifactVersion) {
					if errors.Is(err, errIncompleteArtifact) {
						res.Deferred++
					}
					continue
				}
				if errors.Is(err, errCorruptArtifact) {
					log.Printf("artifact: skipping session %s from %s: %v", gid, origin, err)
					res.Deferred++
					continue
				}
				return res, err
			}
			write := rewriteForImport(m, msgs)
			writeRes, err := database.WriteSessionBatchAtomic([]db.SessionBatchWrite{write})
			if err != nil {
				if errors.Is(err, db.ErrSessionExcluded) || errors.Is(err, db.ErrSessionTrashed) {
					continue
				}
				return res, fmt.Errorf("importing artifact session %s: %w", gid, err)
			}
			res.Sessions += writeRes.WrittenSessions
			res.Messages += writeRes.WrittenMessages
			if _, err := database.ReapplyMetadataReplayState(ctx, gid, write.Session.ID); err != nil {
				return res, fmt.Errorf("reapplying metadata after importing artifact session %s: %w", gid, err)
			}
			if err := database.SetSyncState(stateKey, manifestHash); err != nil {
				return res, fmt.Errorf("writing import state for %s: %w", gid, err)
			}
		}
	}
	return res, nil
}

func importStateKey(origin, gid string) string {
	return importStatePrefix + origin + ":" + gid
}

func validateCheckpoint(cp *checkpoint, origin string) error {
	if cp.Version > formatVersion {
		return fmt.Errorf(
			"%w: checkpoint for %s has artifact version %d",
			errFutureArtifactVersion, origin, cp.Version,
		)
	}
	if cp.Version != formatVersion {
		return fmt.Errorf(
			"checkpoint for %s has unsupported artifact version %d",
			origin, cp.Version,
		)
	}
	if cp.Origin != origin {
		return fmt.Errorf(
			"checkpoint origin mismatch for %s: got %q",
			origin, cp.Origin,
		)
	}
	return validateCheckpointReferences(cp, origin)
}

func validateCheckpointReferences(cp *checkpoint, origin string) error {
	for gid, manifestHash := range cp.Sessions {
		if gid == "" {
			return fmt.Errorf("checkpoint for %s contains empty session id", origin)
		}
		if !strings.HasPrefix(gid, origin+"~") {
			return fmt.Errorf(
				"checkpoint session %s does not belong to origin %s",
				gid, origin,
			)
		}
		if strings.TrimSpace(manifestHash) == "" {
			return fmt.Errorf("checkpoint session %s has empty manifest hash", gid)
		}
		if err := validateHashHex(manifestHash); err != nil {
			return fmt.Errorf("checkpoint session %s has invalid manifest hash: %w", gid, err)
		}
	}
	return nil
}

func validateManifest(m manifest, origin, gid string) error {
	if m.Version > formatVersion {
		return fmt.Errorf(
			"%w: manifest %s has artifact version %d",
			errFutureArtifactVersion, gid, m.Version,
		)
	}
	if m.Version != formatVersion {
		return fmt.Errorf(
			"manifest %s has unsupported artifact version %d",
			gid, m.Version,
		)
	}
	if m.Origin != origin {
		return fmt.Errorf(
			"manifest origin mismatch for %s: got %q",
			gid, m.Origin,
		)
	}
	if m.NativeSessionID == "" {
		return fmt.Errorf("manifest %s has empty native session id", gid)
	}
	expectedGID := origin + "~" + m.NativeSessionID
	if gid != expectedGID {
		return fmt.Errorf(
			"manifest session id mismatch: checkpoint has %s, manifest has %s",
			gid, expectedGID,
		)
	}
	if m.Session.ID != m.NativeSessionID {
		return fmt.Errorf(
			"manifest %s session row id mismatch: got %q",
			gid, m.Session.ID,
		)
	}
	if m.Session.Machine != origin {
		return fmt.Errorf(
			"manifest %s session row machine mismatch: got %q",
			gid, m.Session.Machine,
		)
	}
	if len(m.Segments) == 0 {
		return fmt.Errorf("manifest %s has no message segments", gid)
	}
	if err := validateManifestReferences(m); err != nil {
		return err
	}
	return nil
}

func validateManifestReferences(m manifest) error {
	return validateManifestReferencesWithLimits(m, productionArtifactLimits())
}

func validateManifestReferencesWithLimits(m manifest, limits artifactLimits) error {
	if len(m.Segments) > limits.manifestSegments {
		return fmt.Errorf(
			"manifest segment reference limit exceeded: got %d, limit %d",
			len(m.Segments), limits.manifestSegments,
		)
	}
	seen := make(map[string]struct{}, len(m.Segments))
	for _, segmentHash := range m.Segments {
		if err := validateHashHex(segmentHash); err != nil {
			return fmt.Errorf("manifest segment has invalid hash: %w", err)
		}
		if _, ok := seen[segmentHash]; ok {
			return fmt.Errorf("manifest has duplicate segment reference %s", segmentHash)
		}
		seen[segmentHash] = struct{}{}
	}
	if len(m.UsageEvents) > limits.manifestUsageEvents {
		return fmt.Errorf(
			"manifest usage event limit exceeded: got %d, limit %d",
			len(m.UsageEvents), limits.manifestUsageEvents,
		)
	}
	if m.RawSource != nil && m.RawSource.Hash != "" {
		if err := validateHashHex(m.RawSource.Hash); err != nil {
			return fmt.Errorf("manifest raw source has invalid hash: %w", err)
		}
	}
	return nil
}

func rewriteForImport(m manifest, msgs []db.Message) db.SessionBatchWrite {
	importedID := m.Origin + "~" + m.NativeSessionID
	sess := m.Session.dbSession()
	sess.ID = importedID
	sess.Machine = m.Origin
	sess.SessionName = m.SessionName
	clearImportedSessionSourceState(&sess)
	// Restore signal state dropped from the Session JSON; signalsFromSession
	// reads these fields below to persist the imported session's signal columns.
	sess.HasToolCalls = m.SessionHasToolCalls
	sess.HasContextData = m.SessionHasContextData
	sess.ApplyQualitySignals(m.SessionQualitySignals.dbQualitySignals())
	// Secret findings are not carried in the manifest, so an imported session has
	// no finding rows. Treat it as unscanned rather than trusting the source scan:
	// clear the rules version (json:"-", so already absent) and the leak count
	// (carried in the Session JSON) so the count stays consistent with the zero
	// findings and `secrets scan --backfill` rescans it with local rules. Stamping
	// it scanned-at-source-version would make backfill (secrets_rules_version !=
	// current) skip a secret-bearing session, leaving no revealable findings.
	sess.SecretsRulesVersion = ""
	sess.SecretLeakCount = 0
	sess.SourceSessionID = prefixImportedSessionID(m.Origin, sess.SourceSessionID)
	if sess.ParentSessionID != nil {
		prefixed := prefixImportedSessionID(m.Origin, *sess.ParentSessionID)
		sess.ParentSessionID = &prefixed
	}
	for i := range msgs {
		msgs[i].ID = 0
		msgs[i].SessionID = importedID
		for j := range msgs[i].ToolCalls {
			msgs[i].ToolCalls[j].MessageID = 0
			msgs[i].ToolCalls[j].SessionID = importedID
			msgs[i].ToolCalls[j].SubagentSessionID = prefixImportedSessionID(
				m.Origin,
				msgs[i].ToolCalls[j].SubagentSessionID,
			)
			for k := range msgs[i].ToolCalls[j].ResultEvents {
				ev := &msgs[i].ToolCalls[j].ResultEvents[k]
				ev.SubagentSessionID = prefixImportedSessionID(m.Origin, ev.SubagentSessionID)
			}
		}
	}
	usageEvents := dbUsageEvents(m.UsageEvents, importedID)
	return db.SessionBatchWrite{
		Session:         sess,
		Messages:        msgs,
		UsageEvents:     usageEvents,
		Signals:         signalsFromSession(sess),
		DataVersion:     m.DataVersion,
		ReplaceMessages: true,
	}
}

func clearImportedSessionSourceState(sess *db.Session) {
	sess.FilePath = nil
	sess.FileSize = nil
	sess.FileMtime = nil
	sess.NextOrdinal = 0
	sess.LastEntryUUID = nil
	sess.FileInode = nil
	sess.FileDevice = nil
	sess.FileHash = nil
}

func prefixImportedSessionID(origin, id string) string {
	if id == "" || strings.Contains(id, "~") {
		return id
	}
	return origin + "~" + id
}

func signalsFromSession(s db.Session) db.SessionSignalUpdate {
	update := db.SessionSignalUpdate{
		ToolFailureSignalCount: s.ToolFailureSignalCount,
		ToolRetryCount:         s.ToolRetryCount,
		EditChurnCount:         s.EditChurnCount,
		ConsecutiveFailureMax:  s.ConsecutiveFailureMax,
		Outcome:                s.Outcome,
		OutcomeConfidence:      s.OutcomeConfidence,
		EndedWithRole:          s.EndedWithRole,
		FinalFailureStreak:     s.FinalFailureStreak,
		SignalsPendingSince:    s.SignalsPendingSince,
		CompactionCount:        s.CompactionCount,
		MidTaskCompactionCount: s.MidTaskCompactionCount,
		ContextPressureMax:     s.ContextPressureMax,
		HealthScore:            s.HealthScore,
		HealthGrade:            s.HealthGrade,
		HasToolCalls:           s.HasToolCalls,
		HasContextData:         s.HasContextData,
		SecretLeakCount:        s.SecretLeakCount,
		SecretsRulesVersion:    s.SecretsRulesVersion,
	}
	if qs := s.StoredQualitySignals(); qs != nil {
		update.QualitySignals = *qs
	}
	return update
}

func readLatestCheckpoint(originRoot string) (*checkpoint, error) {
	paths, err := filepath.Glob(filepath.Join(originRoot, "checkpoints", "cp-*.json"))
	if err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		return nil, nil
	}
	sort.Strings(paths)
	data, err := os.ReadFile(paths[len(paths)-1])
	if err != nil {
		return nil, err
	}
	var cp checkpoint
	if err := json.Unmarshal(data, &cp); err != nil {
		return nil, err
	}
	return &cp, nil
}

func readLatestCompatibleCheckpoint(originRoot, origin string) (*checkpoint, error) {
	paths, err := filepath.Glob(filepath.Join(originRoot, "checkpoints", "cp-*.json"))
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	for _, v := range slices.Backward(paths) {
		data, err := os.ReadFile(v)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, err
		}
		var cp checkpoint
		if err := json.Unmarshal(data, &cp); err != nil {
			log.Printf("artifact: skipping corrupt checkpoint %s: %v", v, err)
			quarantineArtifact(v)
			continue
		}
		if err := validateCheckpoint(&cp, origin); err != nil {
			if errors.Is(err, errFutureArtifactVersion) {
				continue
			}
			log.Printf("artifact: skipping invalid checkpoint %s: %v", v, err)
			quarantineArtifact(v)
			continue
		}
		if err := validateCheckpointSequenceIdentity(cp, filepath.Base(v)); err != nil {
			log.Printf("artifact: skipping invalid checkpoint %s: %v", v, err)
			quarantineArtifact(v)
			continue
		}
		return &cp, nil
	}
	return nil, nil
}

func readManifest(originRoot, hash string) (manifest, error) {
	if err := validateHashHex(hash); err != nil {
		return manifest{}, err
	}
	path := filepath.Join(originRoot, "manifests", hash+manifestExtension)
	data, err := readCompressed(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return manifest{}, fmt.Errorf("%w: manifest %s", errIncompleteArtifact, hash)
		}
		if errors.Is(err, errCorruptArtifact) {
			quarantineArtifact(path)
			return manifest{}, fmt.Errorf("manifest %s: %w", hash, err)
		}
		return manifest{}, fmt.Errorf("reading manifest %s: %w", hash, err)
	}
	if got := hashHex(data); got != hash {
		quarantineArtifact(path)
		return manifest{}, fmt.Errorf("%w: manifest %s hash mismatch: got %s", errCorruptArtifact, hash, got)
	}
	m, err := decodeManifestWithLimits(data, productionArtifactLimits())
	if err != nil {
		quarantineArtifact(path)
		return manifest{}, fmt.Errorf("%w: decoding manifest %s: %v", errCorruptArtifact, hash, err)
	}
	return m, nil
}

func decodeManifestWithLimits(data []byte, limits artifactLimits) (manifest, error) {
	var envelope struct {
		Version     int             `json:"v"`
		Origin      string          `json:"origin"`
		Segments    json.RawMessage `json:"segments"`
		UsageEvents json.RawMessage `json:"usage_events"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return manifest{}, err
	}
	// Future manifests are retained for forward compatibility. Reading only
	// their scalar header avoids allocating collections whose schema this
	// version does not understand.
	if envelope.Version > formatVersion {
		return manifest{Version: envelope.Version, Origin: envelope.Origin}, nil
	}
	if err := preflightManifestCollections(
		envelope.Segments, envelope.UsageEvents, limits,
	); err != nil {
		return manifest{}, err
	}
	var m manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return manifest{}, err
	}
	return m, nil
}

func preflightManifestCollections(
	segments, usageEvents json.RawMessage,
	limits artifactLimits,
) error {
	if err := preflightSegmentReferences(segments, limits.manifestSegments); err != nil {
		return err
	}
	return preflightJSONArrayCount(
		usageEvents, "manifest usage event", limits.manifestUsageEvents,
	)
}

func preflightSegmentReferences(data json.RawMessage, limit int) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	token, err := dec.Token()
	if err != nil {
		return err
	}
	if token != json.Delim('[') {
		return errors.New("manifest segments must be an array")
	}
	seen := make(map[string]struct{}, min(limit, 16))
	count := 0
	for dec.More() {
		if count >= limit {
			return fmt.Errorf("manifest segment reference limit exceeded: limit %d", limit)
		}
		var hash string
		if err := dec.Decode(&hash); err != nil {
			return fmt.Errorf("decoding manifest segment reference: %w", err)
		}
		if _, ok := seen[hash]; ok {
			return fmt.Errorf("manifest has duplicate segment reference %s", hash)
		}
		seen[hash] = struct{}{}
		count++
	}
	_, err = dec.Token()
	return err
}

func preflightJSONArrayCount(data json.RawMessage, name string, limit int) error {
	_, err := countJSONArrayElements(data, name, limit)
	return err
}

func countJSONArrayElements(
	data json.RawMessage,
	name string,
	limit int,
) (int, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return 0, nil
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	token, err := dec.Token()
	if err != nil {
		return 0, err
	}
	if token != json.Delim('[') {
		return 0, fmt.Errorf("%ss must be an array", name)
	}
	count := 0
	for dec.More() {
		if count >= limit {
			return 0, fmt.Errorf("%s limit exceeded: limit %d", name, limit)
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return 0, fmt.Errorf("decoding %s: %w", name, err)
		}
		count++
	}
	if _, err := dec.Token(); err != nil {
		return 0, err
	}
	return count, nil
}

func readManifestMessages(originRoot string, m manifest) ([]db.Message, error) {
	return readManifestMessagesWithLimits(originRoot, m, productionArtifactLimits())
}

func readManifestMessagesWithLimits(
	originRoot string,
	m manifest,
	limits artifactLimits,
) ([]db.Message, error) {
	var msgs []db.Message
	err := walkManifestMessagesWithLimits(
		originRoot, m, limits,
		func(segmentMsgs []db.Message) {
			msgs = append(msgs, segmentMsgs...)
		},
	)
	return msgs, err
}

func walkManifestMessagesWithLimits(
	originRoot string,
	m manifest,
	limits artifactLimits,
	visit func([]db.Message),
) error {
	if err := validateManifestReferencesWithLimits(m, limits); err != nil {
		return fmt.Errorf("%w: %v", errCorruptArtifact, err)
	}
	var decodedBytes int64
	totalMessages := 0
	totalNested := nestedCollectionCounts{}
	for _, segmentHash := range m.Segments {
		segment, err := readSegmentPreflightWithLimits(
			originRoot, segmentHash, limits,
		)
		if err != nil {
			return err
		}
		segmentBytes := int64(len(segment.data))
		if segmentBytes > limits.sessionDecodedBytes-decodedBytes {
			return fmt.Errorf(
				"%w: session decoded byte limit exceeded: limit %d",
				errCorruptArtifact, limits.sessionDecodedBytes,
			)
		}
		if len(segment.preflight.records) > limits.sessionMessages-totalMessages {
			return fmt.Errorf(
				"%w: session message limit exceeded: limit %d",
				errCorruptArtifact, limits.sessionMessages,
			)
		}
		if exceedsCollectionLimit(
			totalNested.toolCalls,
			segment.preflight.nested.toolCalls,
			limits.sessionToolCalls,
		) {
			return fmt.Errorf(
				"%w: session tool call limit exceeded: limit %d",
				errCorruptArtifact, limits.sessionToolCalls,
			)
		}
		if exceedsCollectionLimit(
			totalNested.resultEvents,
			segment.preflight.nested.resultEvents,
			limits.sessionResultEvents,
		) {
			return fmt.Errorf(
				"%w: session result event limit exceeded: limit %d",
				errCorruptArtifact, limits.sessionResultEvents,
			)
		}
		segmentMsgs, err := decodeStoredSegment(segment)
		if err != nil {
			return err
		}
		decodedBytes += segmentBytes
		totalMessages += len(segmentMsgs)
		totalNested.toolCalls += segment.preflight.nested.toolCalls
		totalNested.resultEvents += segment.preflight.nested.resultEvents
		if visit != nil {
			visit(segmentMsgs)
		}
	}
	return nil
}

func readSegmentMessages(originRoot, segmentHash string) ([]db.Message, error) {
	msgs, _, err := readSegmentMessagesWithLimits(
		originRoot, segmentHash, productionArtifactLimits(),
	)
	return msgs, err
}

func readSegmentMessagesWithLimits(
	originRoot, segmentHash string,
	limits artifactLimits,
) ([]db.Message, int64, error) {
	segment, err := readSegmentPreflightWithLimits(originRoot, segmentHash, limits)
	if err != nil {
		return nil, 0, err
	}
	segmentMsgs, err := decodeStoredSegment(segment)
	if err != nil {
		return nil, 0, err
	}
	return segmentMsgs, int64(len(segment.data)), nil
}

type storedSegmentPreflight struct {
	path      string
	hash      string
	data      []byte
	preflight segmentPreflight
}

func readSegmentPreflightWithLimits(
	originRoot, segmentHash string,
	limits artifactLimits,
) (storedSegmentPreflight, error) {
	if err := validateHashHex(segmentHash); err != nil {
		return storedSegmentPreflight{}, fmt.Errorf("manifest segment has invalid hash: %w", err)
	}
	path := filepath.Join(originRoot, "segments", segmentHash+segmentExtension)
	data, err := readCompressed(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return storedSegmentPreflight{}, fmt.Errorf(
				"%w: segment %s", errIncompleteArtifact, segmentHash,
			)
		}
		if errors.Is(err, errCorruptArtifact) {
			quarantineArtifact(path)
			return storedSegmentPreflight{}, fmt.Errorf("segment %s: %w", segmentHash, err)
		}
		return storedSegmentPreflight{}, fmt.Errorf("reading segment %s: %w", segmentHash, err)
	}
	if got := hashHex(data); got != segmentHash {
		quarantineArtifact(path)
		return storedSegmentPreflight{}, fmt.Errorf(
			"%w: segment %s hash mismatch: got %s",
			errCorruptArtifact, segmentHash, got,
		)
	}
	preflight, err := preflightSegmentData(data, limits)
	if err != nil {
		if errors.Is(err, errFutureArtifactVersion) {
			return storedSegmentPreflight{}, fmt.Errorf("segment %s: %w", segmentHash, err)
		}
		quarantineArtifact(path)
		return storedSegmentPreflight{}, fmt.Errorf(
			"%w: segment %s: %v", errCorruptArtifact, segmentHash, err,
		)
	}
	return storedSegmentPreflight{
		path: path, hash: segmentHash, data: data, preflight: preflight,
	}, nil
}

func decodeStoredSegment(segment storedSegmentPreflight) ([]db.Message, error) {
	segmentMsgs, err := decodePreflightedSegment(segment.preflight)
	if err == nil {
		return segmentMsgs, nil
	}
	quarantineArtifact(segment.path)
	return nil, fmt.Errorf("%w: segment %s: %v", errCorruptArtifact, segment.hash, err)
}

func canonicalMessages(msgs []db.Message) []db.Message {
	out := make([]db.Message, len(msgs))
	for i, msg := range msgs {
		msg.ID = 0
		msg.SessionID = ""
		if len(msg.ToolCalls) > 0 {
			calls := make([]db.ToolCall, len(msg.ToolCalls))
			copy(calls, msg.ToolCalls)
			for j := range calls {
				calls[j].MessageID = 0
				calls[j].SessionID = ""
			}
			msg.ToolCalls = calls
		}
		out[i] = msg
	}
	return out
}

func canonicalUsageEvents(events []db.UsageEvent) []artifactUsageEvent {
	out := make([]artifactUsageEvent, len(events))
	for i, ev := range events {
		out[i] = artifactUsageEvent{
			MessageOrdinal:           ev.MessageOrdinal,
			Source:                   ev.Source,
			Model:                    ev.Model,
			InputTokens:              ev.InputTokens,
			OutputTokens:             ev.OutputTokens,
			CacheCreationInputTokens: ev.CacheCreationInputTokens,
			CacheReadInputTokens:     ev.CacheReadInputTokens,
			ReasoningTokens:          ev.ReasoningTokens,
			CostUSD:                  ev.CostUSD,
			CostStatus:               ev.CostStatus,
			CostSource:               ev.CostSource,
			OccurredAt:               ev.OccurredAt,
			DedupKey:                 ev.DedupKey,
		}
	}
	return out
}

func validateExportNestedCollections(msgs []db.Message, limits artifactLimits) error {
	total := nestedCollectionCounts{}
	for _, msg := range msgs {
		messageNested, err := dbMessageNestedCounts(msg, limits)
		if err != nil {
			return err
		}
		if err := validateMessageFitsSegment(msg.Ordinal, messageNested, limits); err != nil {
			return err
		}
		if exceedsCollectionLimit(
			total.toolCalls, messageNested.toolCalls, limits.sessionToolCalls,
		) {
			return fmt.Errorf(
				"session tool call limit exceeded at message ordinal %d: limit %d",
				msg.Ordinal, limits.sessionToolCalls,
			)
		}
		if exceedsCollectionLimit(
			total.resultEvents, messageNested.resultEvents, limits.sessionResultEvents,
		) {
			return fmt.Errorf(
				"session result event limit exceeded at message ordinal %d: limit %d",
				msg.Ordinal, limits.sessionResultEvents,
			)
		}
		total.toolCalls += messageNested.toolCalls
		total.resultEvents += messageNested.resultEvents
	}
	return nil
}

func dbMessageNestedCounts(
	msg db.Message,
	limits artifactLimits,
) (nestedCollectionCounts, error) {
	if len(msg.ToolCalls) > limits.messageToolCalls {
		return nestedCollectionCounts{}, fmt.Errorf(
			"tool call limit exceeded for message ordinal %d: got %d, limit %d",
			msg.Ordinal, len(msg.ToolCalls), limits.messageToolCalls,
		)
	}
	counts := nestedCollectionCounts{toolCalls: len(msg.ToolCalls)}
	for toolIndex, call := range msg.ToolCalls {
		if len(call.ResultEvents) > limits.toolResultEvents {
			return nestedCollectionCounts{}, fmt.Errorf(
				"result event limit exceeded for tool call %d in message ordinal %d: got %d, limit %d",
				toolIndex, msg.Ordinal, len(call.ResultEvents), limits.toolResultEvents,
			)
		}
		counts.resultEvents += len(call.ResultEvents)
	}
	return counts, nil
}

func validateMessageFitsSegment(
	ordinal int,
	counts nestedCollectionCounts,
	limits artifactLimits,
) error {
	if counts.toolCalls > limits.segmentToolCalls {
		return fmt.Errorf(
			"message ordinal %d cannot fit in one segment: got %d tool calls, segment limit %d",
			ordinal, counts.toolCalls, limits.segmentToolCalls,
		)
	}
	if counts.resultEvents > limits.segmentResultEvents {
		return fmt.Errorf(
			"message ordinal %d cannot fit in one segment: got %d result events, segment limit %d",
			ordinal, counts.resultEvents, limits.segmentResultEvents,
		)
	}
	return nil
}

func encodeSegment(msgs []db.Message) ([]byte, error) {
	var buf bytes.Buffer
	for _, msg := range msgs {
		data, err := canonicalJSON(segmentMessageFromDB(msg))
		if err != nil {
			return nil, fmt.Errorf("encoding message segment: %w", err)
		}
		buf.Write(data)
	}
	return buf.Bytes(), nil
}

// forEachEncodedSegment emits deterministic NDJSON chunks while bounding each
// chunk to the size accepted by readers. Chunks normally stop near
// segmentTargetSize; a single larger record remains intact up to the hard
// readable limit so message records are never split.
func forEachEncodedSegmentWithLimits(
	msgs []db.Message,
	limits artifactLimits,
	emit func([]byte) error,
) error {
	var buf bytes.Buffer
	segmentMessages := 0
	segmentNested := nestedCollectionCounts{}
	flush := func() error {
		if err := emit(buf.Bytes()); err != nil {
			return err
		}
		buf.Reset()
		segmentMessages = 0
		segmentNested = nestedCollectionCounts{}
		return nil
	}
	for _, msg := range msgs {
		messageNested, err := dbMessageNestedCounts(msg, limits)
		if err != nil {
			return err
		}
		if err := validateMessageFitsSegment(msg.Ordinal, messageNested, limits); err != nil {
			return err
		}
		data, err := canonicalJSON(segmentMessageFromDB(msg))
		if err != nil {
			return fmt.Errorf("encoding message segment: %w", err)
		}
		if int64(len(data)) > segmentDecodedLimit {
			return fmt.Errorf(
				"encoded message record at ordinal %d exceeds %d-byte readable limit",
				msg.Ordinal, segmentDecodedLimit,
			)
		}
		if buf.Len() > 0 && (int64(buf.Len()+len(data)) > segmentTargetSize ||
			segmentMessages >= limits.segmentMessages ||
			exceedsCollectionLimit(
				segmentNested.toolCalls,
				messageNested.toolCalls,
				limits.segmentToolCalls,
			) || exceedsCollectionLimit(
			segmentNested.resultEvents,
			messageNested.resultEvents,
			limits.segmentResultEvents,
		)) {
			if err := flush(); err != nil {
				return err
			}
		}
		_, _ = buf.Write(data)
		segmentMessages++
		segmentNested.toolCalls += messageNested.toolCalls
		segmentNested.resultEvents += messageNested.resultEvents
	}
	if buf.Len() > 0 || len(msgs) == 0 {
		return flush()
	}
	return nil
}

func decodeSegment(data []byte) ([]db.Message, error) {
	return decodeSegmentWithLimits(data, productionArtifactLimits())
}

func decodeSegmentWithLimits(data []byte, limits artifactLimits) ([]db.Message, error) {
	preflight, err := preflightSegmentData(data, limits)
	if err != nil {
		return nil, err
	}
	return decodePreflightedSegment(preflight)
}

func decodePreflightedSegment(preflight segmentPreflight) ([]db.Message, error) {
	msgs := make([]db.Message, 0, len(preflight.records))
	for _, line := range preflight.records {
		var record segmentMessage
		if err := json.Unmarshal(line, &record); err != nil {
			return nil, fmt.Errorf("decoding message segment: %w", err)
		}
		msgs = append(msgs, record.dbMessage())
	}
	return msgs, nil
}

func segmentRecords(data []byte, limit int) ([][]byte, error) {
	capacity := min(max(limit, 0), 64)
	records := make([][]byte, 0, capacity)
	remaining := data
	lineNumber := 0
	for len(remaining) > 0 {
		lineNumber++
		newline := bytes.IndexByte(remaining, '\n')
		line := remaining
		if newline >= 0 {
			line = remaining[:newline]
			remaining = remaining[newline+1:]
		} else {
			remaining = nil
		}
		if len(records) >= limit {
			return nil, fmt.Errorf(
				"message record limit exceeded: limit %d per segment", limit,
			)
		}
		if len(bytes.TrimSpace(line)) == 0 {
			return nil, fmt.Errorf("blank message record at line %d", lineNumber)
		}
		records = append(records, line)
	}
	return records, nil
}

func preflightSegmentData(data []byte, limits artifactLimits) (segmentPreflight, error) {
	records, err := segmentRecords(data, limits.segmentMessages)
	if err != nil {
		return segmentPreflight{}, err
	}
	preflight := segmentPreflight{records: records}
	for _, line := range records {
		var header struct {
			Version int `json:"v"`
		}
		if err := json.Unmarshal(line, &header); err != nil {
			return segmentPreflight{}, fmt.Errorf("decoding message segment header: %w", err)
		}
		if header.Version > formatVersion {
			return segmentPreflight{}, fmt.Errorf(
				"%w: message segment has artifact version %d",
				errFutureArtifactVersion, header.Version,
			)
		}
		if header.Version != formatVersion {
			return segmentPreflight{}, fmt.Errorf(
				"message segment has unsupported artifact version %d",
				header.Version,
			)
		}
		messageNested, err := preflightMessageNestedCollections(line, limits)
		if err != nil {
			return segmentPreflight{}, err
		}
		if exceedsCollectionLimit(
			preflight.nested.toolCalls,
			messageNested.toolCalls,
			limits.segmentToolCalls,
		) {
			return segmentPreflight{}, fmt.Errorf(
				"segment tool call limit exceeded: limit %d", limits.segmentToolCalls,
			)
		}
		if exceedsCollectionLimit(
			preflight.nested.resultEvents,
			messageNested.resultEvents,
			limits.segmentResultEvents,
		) {
			return segmentPreflight{}, fmt.Errorf(
				"segment result event limit exceeded: limit %d",
				limits.segmentResultEvents,
			)
		}
		preflight.nested.toolCalls += messageNested.toolCalls
		preflight.nested.resultEvents += messageNested.resultEvents
	}
	return preflight, nil
}

func preflightMessageNestedCollections(
	line []byte,
	limits artifactLimits,
) (nestedCollectionCounts, error) {
	var envelope struct {
		Ordinal   int             `json:"ordinal"`
		ToolCalls json.RawMessage `json:"tool_calls"`
	}
	if err := json.Unmarshal(line, &envelope); err != nil {
		return nestedCollectionCounts{}, fmt.Errorf(
			"decoding message segment collections: %w", err,
		)
	}
	return preflightToolCallCollections(envelope.ToolCalls, envelope.Ordinal, limits)
}

func preflightToolCallCollections(
	data json.RawMessage,
	ordinal int,
	limits artifactLimits,
) (nestedCollectionCounts, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nestedCollectionCounts{}, nil
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	token, err := dec.Token()
	if err != nil {
		return nestedCollectionCounts{}, err
	}
	if token != json.Delim('[') {
		return nestedCollectionCounts{}, errors.New("message tool_calls must be an array")
	}
	counts := nestedCollectionCounts{}
	for dec.More() {
		if counts.toolCalls >= limits.messageToolCalls {
			return nestedCollectionCounts{}, fmt.Errorf(
				"tool call limit exceeded for message ordinal %d: limit %d per message",
				ordinal, limits.messageToolCalls,
			)
		}
		var toolEnvelope struct {
			ResultEvents json.RawMessage `json:"result_events"`
		}
		if err := dec.Decode(&toolEnvelope); err != nil {
			return nestedCollectionCounts{}, fmt.Errorf(
				"decoding tool call %d in message ordinal %d: %w",
				counts.toolCalls, ordinal, err,
			)
		}
		resultEvents, err := countJSONArrayElements(
			toolEnvelope.ResultEvents, "result event", limits.toolResultEvents,
		)
		if err != nil {
			return nestedCollectionCounts{}, fmt.Errorf(
				"preflighting tool call %d in message ordinal %d: %w",
				counts.toolCalls, ordinal, err,
			)
		}
		counts.toolCalls++
		counts.resultEvents += resultEvents
	}
	if _, err := dec.Token(); err != nil {
		return nestedCollectionCounts{}, err
	}
	return counts, nil
}

func dbUsageEvents(events []artifactUsageEvent, sessionID string) []db.UsageEvent {
	out := make([]db.UsageEvent, len(events))
	for i, ev := range events {
		out[i] = db.UsageEvent{
			SessionID:                sessionID,
			MessageOrdinal:           ev.MessageOrdinal,
			Source:                   ev.Source,
			Model:                    ev.Model,
			InputTokens:              ev.InputTokens,
			OutputTokens:             ev.OutputTokens,
			CacheCreationInputTokens: ev.CacheCreationInputTokens,
			CacheReadInputTokens:     ev.CacheReadInputTokens,
			ReasoningTokens:          ev.ReasoningTokens,
			CostUSD:                  ev.CostUSD,
			CostStatus:               ev.CostStatus,
			CostSource:               ev.CostSource,
			OccurredAt:               ev.OccurredAt,
			DedupKey:                 ev.DedupKey,
		}
	}
	return out
}

func segmentMessageFromDB(msg db.Message) segmentMessage {
	record := segmentMessage{
		Version:           formatVersion,
		Ordinal:           msg.Ordinal,
		Role:              msg.Role,
		Content:           msg.Content,
		ThinkingText:      msg.ThinkingText,
		Timestamp:         msg.Timestamp,
		HasThinking:       msg.HasThinking,
		HasToolUse:        msg.HasToolUse,
		ContentLength:     msg.ContentLength,
		Model:             msg.Model,
		TokenUsage:        msg.TokenUsage,
		ContextTokens:     msg.ContextTokens,
		OutputTokens:      msg.OutputTokens,
		HasContextTokens:  msg.HasContextTokens,
		HasOutputTokens:   msg.HasOutputTokens,
		ClaudeMessageID:   msg.ClaudeMessageID,
		ClaudeRequestID:   msg.ClaudeRequestID,
		IsSystem:          msg.IsSystem,
		SourceType:        msg.SourceType,
		SourceSubtype:     msg.SourceSubtype,
		SourceUUID:        msg.SourceUUID,
		SourceParentUUID:  msg.SourceParentUUID,
		IsSidechain:       msg.IsSidechain,
		IsCompactBoundary: msg.IsCompactBoundary,
	}
	if len(msg.ToolCalls) > 0 {
		record.ToolCalls = make([]segmentToolCall, len(msg.ToolCalls))
		for i, call := range msg.ToolCalls {
			record.ToolCalls[i] = segmentToolCall{
				CallIndex:           i,
				ToolName:            call.ToolName,
				Category:            call.Category,
				ToolUseID:           call.ToolUseID,
				InputJSON:           call.InputJSON,
				FilePath:            call.FilePath,
				SkillName:           call.SkillName,
				ResultContentLength: call.ResultContentLength,
				ResultContent:       call.ResultContent,
				SubagentSessionID:   call.SubagentSessionID,
			}
			if len(call.ResultEvents) > 0 {
				record.ToolCalls[i].ResultEvents = make([]segmentResultEvent, len(call.ResultEvents))
				for j, ev := range call.ResultEvents {
					record.ToolCalls[i].ResultEvents[j] = segmentResultEvent{
						ToolUseID:         ev.ToolUseID,
						AgentID:           ev.AgentID,
						SubagentSessionID: ev.SubagentSessionID,
						Source:            ev.Source,
						Status:            ev.Status,
						Content:           ev.Content,
						ContentLength:     ev.ContentLength,
						Timestamp:         ev.Timestamp,
						EventIndex:        ev.EventIndex,
					}
				}
			}
		}
	}
	return record
}

func (m segmentMessage) dbMessage() db.Message {
	msg := db.Message{
		Ordinal:           m.Ordinal,
		Role:              m.Role,
		Content:           m.Content,
		ThinkingText:      m.ThinkingText,
		Timestamp:         m.Timestamp,
		HasThinking:       m.HasThinking,
		HasToolUse:        m.HasToolUse,
		ContentLength:     m.ContentLength,
		Model:             m.Model,
		TokenUsage:        m.TokenUsage,
		ContextTokens:     m.ContextTokens,
		OutputTokens:      m.OutputTokens,
		HasContextTokens:  m.HasContextTokens,
		HasOutputTokens:   m.HasOutputTokens,
		ClaudeMessageID:   m.ClaudeMessageID,
		ClaudeRequestID:   m.ClaudeRequestID,
		IsSystem:          m.IsSystem,
		SourceType:        m.SourceType,
		SourceSubtype:     m.SourceSubtype,
		SourceUUID:        m.SourceUUID,
		SourceParentUUID:  m.SourceParentUUID,
		IsSidechain:       m.IsSidechain,
		IsCompactBoundary: m.IsCompactBoundary,
	}
	if len(m.ToolCalls) > 0 {
		msg.ToolCalls = make([]db.ToolCall, len(m.ToolCalls))
		for i, call := range m.ToolCalls {
			msg.ToolCalls[i] = db.ToolCall{
				ToolName:            call.ToolName,
				Category:            call.Category,
				ToolUseID:           call.ToolUseID,
				InputJSON:           call.InputJSON,
				FilePath:            call.FilePath,
				SkillName:           call.SkillName,
				ResultContentLength: call.ResultContentLength,
				ResultContent:       call.ResultContent,
				SubagentSessionID:   call.SubagentSessionID,
			}
			if len(call.ResultEvents) > 0 {
				msg.ToolCalls[i].ResultEvents = make([]db.ToolResultEvent, len(call.ResultEvents))
				for j, ev := range call.ResultEvents {
					msg.ToolCalls[i].ResultEvents[j] = db.ToolResultEvent{
						ToolUseID:         ev.ToolUseID,
						AgentID:           ev.AgentID,
						SubagentSessionID: ev.SubagentSessionID,
						Source:            ev.Source,
						Status:            ev.Status,
						Content:           ev.Content,
						ContentLength:     ev.ContentLength,
						Timestamp:         ev.Timestamp,
						EventIndex:        ev.EventIndex,
					}
				}
			}
		}
	}
	return msg
}

func canonicalJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	if err := writeCanonicalJSON(&buf, reflect.ValueOf(v)); err != nil {
		return nil, fmt.Errorf("encoding canonical artifact JSON: %w", err)
	}
	buf.WriteByte('\n')
	return buf.Bytes(), nil
}

func writeCanonicalJSON(buf *bytes.Buffer, v reflect.Value) error {
	if !v.IsValid() {
		buf.WriteString("null")
		return nil
	}
	if v.Kind() == reflect.Interface {
		if v.IsNil() {
			buf.WriteString("null")
			return nil
		}
		return writeCanonicalJSON(buf, v.Elem())
	}
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			buf.WriteString("null")
			return nil
		}
		return writeCanonicalJSON(buf, v.Elem())
	}
	if v.Type() == reflect.TypeFor[json.RawMessage]() {
		raw := v.Interface().(json.RawMessage)
		if len(raw) == 0 {
			buf.WriteString("null")
			return nil
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		var decoded any
		if err := dec.Decode(&decoded); err != nil {
			return err
		}
		return writeCanonicalJSON(buf, reflect.ValueOf(decoded))
	}
	if v.Type() == reflect.TypeFor[json.Number]() {
		buf.WriteString(v.Interface().(json.Number).String())
		return nil
	}
	switch v.Kind() {
	case reflect.Bool:
		buf.WriteString(strconv.FormatBool(v.Bool()))
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		buf.WriteString(strconv.FormatInt(v.Int(), 10))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		buf.WriteString(strconv.FormatUint(v.Uint(), 10))
	case reflect.Float32, reflect.Float64:
		data, err := json.Marshal(v.Interface())
		if err != nil {
			return err
		}
		buf.Write(data)
	case reflect.String:
		data, err := json.Marshal(v.String())
		if err != nil {
			return err
		}
		buf.Write(data)
	case reflect.Slice, reflect.Array:
		buf.WriteByte('[')
		for i := 0; i < v.Len(); i++ {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeCanonicalJSON(buf, v.Index(i)); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case reflect.Map:
		return writeCanonicalMap(buf, v)
	case reflect.Struct:
		return writeCanonicalStruct(buf, v)
	default:
		return fmt.Errorf("unsupported canonical JSON kind %s", v.Kind())
	}
	return nil
}

func writeCanonicalMap(buf *bytes.Buffer, v reflect.Value) error {
	if v.IsNil() {
		buf.WriteString("null")
		return nil
	}
	if v.Type().Key().Kind() != reflect.String {
		return fmt.Errorf("unsupported canonical map key type %s", v.Type().Key())
	}
	keys := make([]string, 0, v.Len())
	for _, key := range v.MapKeys() {
		keys = append(keys, key.String())
	}
	sort.Strings(keys)
	buf.WriteByte('{')
	for i, key := range keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		keyData, err := json.Marshal(key)
		if err != nil {
			return err
		}
		buf.Write(keyData)
		buf.WriteByte(':')
		if err := writeCanonicalJSON(buf, v.MapIndex(reflect.ValueOf(key))); err != nil {
			return err
		}
	}
	buf.WriteByte('}')
	return nil
}

type canonicalField struct {
	name  string
	value reflect.Value
}

func writeCanonicalStruct(buf *bytes.Buffer, v reflect.Value) error {
	fields := make([]canonicalField, 0, v.NumField())
	t := v.Type()
	for i := 0; i < v.NumField(); i++ {
		field := t.Field(i)
		if field.PkgPath != "" {
			continue
		}
		name, omitEmpty, skip := jsonField(field)
		if skip {
			continue
		}
		value := v.Field(i)
		if omitEmpty && isCanonicalEmpty(value) {
			continue
		}
		fields = append(fields, canonicalField{name: name, value: value})
	}
	sort.Slice(fields, func(i, j int) bool {
		return fields[i].name < fields[j].name
	})

	buf.WriteByte('{')
	for i, field := range fields {
		if i > 0 {
			buf.WriteByte(',')
		}
		name, err := json.Marshal(field.name)
		if err != nil {
			return err
		}
		buf.Write(name)
		buf.WriteByte(':')
		if err := writeCanonicalJSON(buf, field.value); err != nil {
			return err
		}
	}
	buf.WriteByte('}')
	return nil
}

func jsonField(field reflect.StructField) (name string, omitEmpty bool, skip bool) {
	name = field.Name
	tag := field.Tag.Get("json")
	if tag == "-" {
		return "", false, true
	}
	if tag == "" {
		return name, false, false
	}
	parts := strings.Split(tag, ",")
	if parts[0] != "" {
		name = parts[0]
	}
	for _, opt := range parts[1:] {
		if opt == "omitempty" {
			omitEmpty = true
		}
	}
	return name, omitEmpty, false
}

func isCanonicalEmpty(v reflect.Value) bool {
	if !v.IsValid() {
		return true
	}
	switch v.Kind() {
	case reflect.Array:
		return v.Len() == 0
	case reflect.Map, reflect.Slice, reflect.String:
		return v.Len() == 0
	case reflect.Bool:
		return !v.Bool()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return v.Int() == 0
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return v.Uint() == 0
	case reflect.Float32, reflect.Float64:
		return v.Float() == 0
	case reflect.Interface, reflect.Pointer:
		return v.IsNil()
	}
	return false
}

func hashHex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func writeCompressed(path string, data []byte) error {
	var buf bytes.Buffer
	enc, err := zstd.NewWriter(&buf)
	if err != nil {
		return err
	}
	if _, err := enc.Write(data); err != nil {
		enc.Close()
		return err
	}
	if err := enc.Close(); err != nil {
		return err
	}
	return writeFileAtomic(path, buf.Bytes(), 0o644)
}

func readCompressed(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	limit, err := compressedArtifactDecodedLimit(path)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errCorruptArtifact, err)
	}
	out, err := readCompressedBytes(data, limit)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errCorruptArtifact, err)
	}
	return out, nil
}

func compressedArtifactDecodedLimit(path string) (int64, error) {
	switch {
	case strings.HasSuffix(path, manifestExtension):
		return manifestDecodedLimit, nil
	case strings.HasSuffix(path, segmentExtension):
		return segmentDecodedLimit, nil
	default:
		return 0, fmt.Errorf("unknown compressed artifact extension for %s", filepath.Base(path))
	}
}

// errArtifactPathConflict reports that an immutable artifact path already
// holds different content. Callers detect it with errors.Is.
var errArtifactPathConflict = errors.New("artifact path conflict")

// quarantineArtifact renames a corrupt artifact aside so read paths treat it
// as missing and a valid copy can be re-fetched under the original name. The
// rename is best-effort: on failure the file stays in place and is rescanned.
func quarantineArtifact(path string) {
	dst := path + quarantineSuffix
	_ = os.Remove(dst)
	err := os.Rename(path, dst)
	switch {
	case err == nil:
		log.Printf("artifact: quarantined corrupt artifact %s", path)
	case !errors.Is(err, fs.ErrNotExist):
		log.Printf("artifact: quarantining %s: %v", path, err)
	}
}

func writeFileAtomic(path string, data []byte, perm fs.FileMode) error {
	if done, err := existingArtifactMatches(path, data); err != nil || done {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), tempFilePrefix+"*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if done, err := existingArtifactMatches(path, data); err != nil || done {
		return err
	}
	if writeFileAtomicBeforeCommit != nil {
		writeFileAtomicBeforeCommit(path)
	}
	if err := writeFileAtomicLink(tmpName, path); err != nil {
		if errors.Is(err, fs.ErrExist) {
			if done, matchErr := existingArtifactMatches(path, data); matchErr != nil || done {
				return matchErr
			}
			return fmt.Errorf("%w at %s", errArtifactPathConflict, path)
		}
		if isHardLinkUnsupported(err) {
			return writeFileNoReplace(path, data, perm)
		}
		return err
	}
	return nil
}

func isHardLinkUnsupported(err error) bool {
	return errors.Is(err, syscall.ENOTSUP) ||
		errors.Is(err, syscall.EOPNOTSUPP) ||
		errors.Is(err, syscall.EXDEV) ||
		errors.Is(err, syscall.EPERM)
}

func writeFileNoReplace(path string, data []byte, perm fs.FileMode) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			if done, matchErr := existingArtifactMatches(path, data); matchErr != nil || done {
				return matchErr
			}
			return fmt.Errorf("%w at %s", errArtifactPathConflict, path)
		}
		return err
	}
	created := true
	defer func() {
		if created {
			_ = os.Remove(path)
		}
	}()
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Chmod(perm); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	created = false
	return nil
}

func existingArtifactMatches(path string, data []byte) (bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	if info.IsDir() {
		return false, fmt.Errorf("artifact destination %s is a directory", path)
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("artifact destination %s is not a regular file", path)
	}
	existing, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	if bytes.Equal(existing, data) {
		return true, nil
	}
	return false, fmt.Errorf("%w at %s", errArtifactPathConflict, path)
}

// CopyUnion copies files from src into dst without deleting files that only
// exist in dst. Existing identical files are left in place.
func CopyUnion(src, dst string) error {
	srcRoot, err := openArtifactRoot(src, "source")
	if err != nil {
		return err
	}
	defer srcRoot.Close()
	dstRoot, err := openArtifactRoot(dst, "destination")
	if err != nil {
		return err
	}
	defer dstRoot.Close()
	if err := validateDisjointRoots(srcRoot.Name(), dstRoot.Name()); err != nil {
		return err
	}

	return fs.WalkDir(srcRoot.FS(), ".", func(path string, ent fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if isTempArtifactEntry(ent.Name()) {
			if ent.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !ent.IsDir() && strings.HasSuffix(ent.Name(), quarantineSuffix) {
			return nil
		}
		if path == "." {
			return nil
		}
		rel := filepath.FromSlash(path)
		if ent.IsDir() {
			return dstRoot.MkdirAll(rel, 0o755)
		}
		info, err := ent.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf(
				"artifact source %s is not a regular file",
				filepath.Join(srcRoot.Name(), rel),
			)
		}
		return copyUnionFile(srcRoot, dstRoot, rel)
	})
}

func openArtifactRoot(path, role string) (*os.Root, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("artifact %s root %s is not a directory", role, path)
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, fmt.Errorf("opening artifact %s root: %w", role, err)
	}
	openedInfo, err := root.Stat(".")
	if err != nil {
		_ = root.Close()
		return nil, fmt.Errorf("stating artifact %s root: %w", role, err)
	}
	currentInfo, err := os.Lstat(path)
	if err != nil {
		_ = root.Close()
		return nil, err
	}
	if !currentInfo.IsDir() || !os.SameFile(openedInfo, currentInfo) {
		_ = root.Close()
		return nil, fmt.Errorf("artifact %s root %s changed while opening", role, path)
	}
	return root, nil
}

func openArtifactSubroot(parent *os.Root, rel, role string) (*os.Root, error) {
	info, err := parent.Lstat(rel)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("artifact %s %s is not a directory", role, rel)
	}
	root, err := parent.OpenRoot(rel)
	if err != nil {
		return nil, fmt.Errorf("opening artifact %s: %w", role, err)
	}
	openedInfo, err := root.Stat(".")
	if err != nil {
		_ = root.Close()
		return nil, fmt.Errorf("stating artifact %s: %w", role, err)
	}
	currentInfo, err := parent.Lstat(rel)
	if err != nil {
		_ = root.Close()
		return nil, err
	}
	if !currentInfo.IsDir() || !os.SameFile(openedInfo, currentInfo) {
		_ = root.Close()
		return nil, fmt.Errorf("artifact %s %s changed while opening", role, rel)
	}
	return root, nil
}

// copyUnionFile copies one artifact file into the destination store. Corrupt
// content-addressed artifacts are never mirrored: an invalid source is skipped
// and an invalid destination under a valid source's name is repaired, so one
// bad file cannot spread between stores or wedge the exchange.
func copyUnionFile(srcRoot, dstRoot *os.Root, rel string) error {
	path := filepath.Join(srcRoot.Name(), rel)
	srcData, info, err := readRootRegularFile(srcRoot, rel)
	if err != nil {
		return err
	}
	to := filepath.Join(dstRoot.Name(), rel)
	existing, err := dstRoot.Lstat(rel)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err == nil {
		if existing.IsDir() {
			return fmt.Errorf("artifact destination %s is a directory", to)
		}
		if !existing.Mode().IsRegular() {
			return fmt.Errorf("artifact destination %s is not a regular file", to)
		}
		if existing.Size() == info.Size() {
			dstData, err := dstRoot.ReadFile(rel)
			if err != nil {
				return err
			}
			if bytes.Equal(srcData, dstData) {
				return nil
			}
		}
		return reconcileArtifactConflict(path, srcData, dstRoot, rel, info.Mode().Perm())
	}
	if known, verr := validateKnownArtifact(rel, srcData); known && verr != nil {
		// Quarantine at detection instead of leaving the corrupt file in
		// place: the export "unchanged" fast path only checks that the
		// content-addressed files exist, so an owned artifact left corrupt
		// would never be regenerated, and an imported one would never be
		// re-fetched. Renaming it away lets both recover on the next round.
		log.Printf("artifact: not mirroring corrupt artifact %s: %v", path, verr)
		quarantineArtifactRoot(srcRoot, rel)
		return nil
	}
	return writeFileAtomicRoot(dstRoot, rel, srcData, info.Mode().Perm())
}

func readRootRegularFile(root *os.Root, rel string) ([]byte, fs.FileInfo, error) {
	file, err := root.Open(rel)
	if err != nil {
		return nil, nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, nil, fmt.Errorf(
			"artifact source %s is not a regular file", filepath.Join(root.Name(), rel),
		)
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return nil, nil, err
	}
	return data, info, nil
}

func quarantineArtifactRoot(root *os.Root, rel string) {
	dst := rel + quarantineSuffix
	_ = root.Remove(dst)
	err := root.Rename(rel, dst)
	path := filepath.Join(root.Name(), rel)
	switch {
	case err == nil:
		log.Printf("artifact: quarantined corrupt artifact %s", path)
	case !errors.Is(err, fs.ErrNotExist):
		log.Printf("artifact: quarantining %s: %v", path, err)
	}
}

// reconcileArtifactConflict handles a same-name, different-content pair. For a
// recognized artifact whose source validates and destination does not, the
// destination is repaired in place; a corrupt source is skipped instead of
// mirrored. Everything else keeps the write-once conflict error.
func reconcileArtifactConflict(
	path string,
	srcData []byte,
	dstRoot *os.Root,
	rel string,
	perm fs.FileMode,
) error {
	to := filepath.Join(dstRoot.Name(), rel)
	known, srcErr := validateKnownArtifact(rel, srcData)
	if !known {
		return fmt.Errorf("%w at %s", errArtifactPathConflict, to)
	}
	if srcErr != nil {
		log.Printf("artifact: not mirroring corrupt artifact %s: %v", path, srcErr)
		return nil
	}
	dstData, err := dstRoot.ReadFile(rel)
	if err != nil {
		return err
	}
	if _, dstErr := validateKnownArtifact(rel, dstData); dstErr != nil {
		log.Printf("artifact: repairing corrupt artifact %s from %s", to, path)
		return replaceFileAtomicRoot(dstRoot, rel, srcData, perm)
	}
	return fmt.Errorf("%w at %s", errArtifactPathConflict, to)
}

func writeFileAtomicRoot(root *os.Root, rel string, data []byte, perm fs.FileMode) error {
	if done, err := existingArtifactMatchesRoot(root, rel, data); err != nil || done {
		return err
	}
	if err := root.MkdirAll(filepath.Dir(rel), 0o755); err != nil {
		return err
	}
	tmp, tmpRel, err := createRootTemp(root, filepath.Dir(rel))
	if err != nil {
		return err
	}
	defer func() { _ = root.Remove(tmpRel) }()
	if err := writeAndCloseArtifact(tmp, data, perm); err != nil {
		return err
	}
	if done, err := existingArtifactMatchesRoot(root, rel, data); err != nil || done {
		return err
	}
	if err := root.Link(tmpRel, rel); err != nil {
		if errors.Is(err, fs.ErrExist) {
			if done, matchErr := existingArtifactMatchesRoot(root, rel, data); matchErr != nil || done {
				return matchErr
			}
			return fmt.Errorf("%w at %s", errArtifactPathConflict, filepath.Join(root.Name(), rel))
		}
		if isHardLinkUnsupported(err) {
			return writeFileNoReplaceRoot(root, rel, data, perm)
		}
		return err
	}
	return nil
}

func replaceFileAtomicRoot(root *os.Root, rel string, data []byte, perm fs.FileMode) error {
	if err := root.MkdirAll(filepath.Dir(rel), 0o755); err != nil {
		return err
	}
	tmp, tmpRel, err := createRootTemp(root, filepath.Dir(rel))
	if err != nil {
		return err
	}
	defer func() { _ = root.Remove(tmpRel) }()
	if err := writeAndCloseArtifact(tmp, data, perm); err != nil {
		return err
	}
	return root.Rename(tmpRel, rel)
}

func createRootTemp(root *os.Root, dir string) (*os.File, string, error) {
	for range 100 {
		var suffix [8]byte
		if _, err := rand.Read(suffix[:]); err != nil {
			return nil, "", err
		}
		rel := filepath.Join(dir, tempFilePrefix+hex.EncodeToString(suffix[:]))
		file, err := root.OpenFile(rel, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			return file, rel, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, "", err
		}
	}
	return nil, "", errors.New("creating temporary artifact file: too many collisions")
}

func writeAndCloseArtifact(file *os.File, data []byte, perm fs.FileMode) error {
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Chmod(perm); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func writeFileNoReplaceRoot(root *os.Root, rel string, data []byte, perm fs.FileMode) error {
	file, err := root.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			if done, matchErr := existingArtifactMatchesRoot(root, rel, data); matchErr != nil || done {
				return matchErr
			}
			return fmt.Errorf("%w at %s", errArtifactPathConflict, filepath.Join(root.Name(), rel))
		}
		return err
	}
	return writeAndCloseArtifact(file, data, perm)
}

func existingArtifactMatchesRoot(root *os.Root, rel string, data []byte) (bool, error) {
	info, err := root.Lstat(rel)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	path := filepath.Join(root.Name(), rel)
	if info.IsDir() {
		return false, fmt.Errorf("artifact destination %s is a directory", path)
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("artifact destination %s is not a regular file", path)
	}
	existing, err := root.ReadFile(rel)
	if err != nil {
		return false, err
	}
	if bytes.Equal(existing, data) {
		return true, nil
	}
	return false, fmt.Errorf("%w at %s", errArtifactPathConflict, path)
}

// validateKnownArtifact validates one store file's bytes against its
// path-derived identity. known is false when rel does not name a recognized
// origin/kind/name artifact; such files are not validatable and are mirrored
// as-is for forward compatibility.
func validateKnownArtifact(rel string, data []byte) (known bool, err error) {
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) != 3 {
		return false, nil
	}
	spec, err := artifactSpecForKind(parts[0], parts[1], parts[2])
	if err != nil {
		return false, nil
	}
	return true, validateArtifactData(spec, data)
}

func isTempArtifactEntry(name string) bool {
	return strings.HasPrefix(name, tempFilePrefix)
}

// IsFolderTarget reports whether target is a local filesystem target rather
// than a future HTTP or object-store target.
func IsFolderTarget(target string) bool {
	if target == "" || strings.Contains(target, "://") {
		return false
	}
	if isWindowsDrivePath(target) {
		return true
	}
	_, _, err := net.SplitHostPort(target)
	return err != nil
}

func isWindowsDrivePath(target string) bool {
	if len(target) < 3 || target[1] != ':' {
		return false
	}
	c := target[0]
	if (c < 'A' || c > 'Z') && (c < 'a' || c > 'z') {
		return false
	}
	return target[2] == '\\' || target[2] == '/'
}
