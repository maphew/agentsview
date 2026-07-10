package artifact

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

const metadataEventExtension = ".json"

// errOriginNotAdopted reports that this machine has no artifact origin, i.e.
// it never opted into artifact sync via `sync --init`, a sync run, or a peer
// exchange. Recording paths treat it as "stay local"; explicit sync paths
// treat it as a hard error.
var errOriginNotAdopted = errors.New("artifact origin not adopted")

type metadataSuppressionKey struct{}

// Metadata operation names written into the metadata event ledger.
const (
	MetadataOpRename     = "rename"
	MetadataOpSoftDelete = "soft_delete"
	MetadataOpRestore    = "restore"
	MetadataOpStar       = "star"
	MetadataOpUnstar     = "unstar"
	MetadataOpPin        = "pin"
	MetadataOpUnpin      = "unpin"
	MetadataOpPurge      = "purge"
)

// MetadataPin identifies a pinned message with stable source coordinates.
type MetadataPin struct {
	SourceUUID string  `json:"source_uuid,omitempty"`
	Ordinal    int     `json:"ordinal"`
	Note       *string `json:"note,omitempty"`
}

// MetadataEventInput describes a local user metadata mutation to append.
type MetadataEventInput struct {
	SessionID string
	Op        string
	Value     json.RawMessage
	Pin       *MetadataPin
}

// MetadataRecord describes a metadata artifact written to disk.
type MetadataRecord struct {
	HLC        string
	Origin     string
	SessionGID string
	Op         string
	Hash       string
	Path       string
}

// MetadataPublishedError reports that the artifact file was durably written,
// but local replay bookkeeping failed afterward.
type MetadataPublishedError struct {
	Record MetadataRecord
	Err    error
}

func (e *MetadataPublishedError) Error() string {
	return fmt.Sprintf("metadata event published but local replay state was not recorded: %v", e.Err)
}

func (e *MetadataPublishedError) Unwrap() error {
	return e.Err
}

// MetadataRecorderOptions configures metadata event artifact writes.
type MetadataRecorderOptions struct {
	DataDir  string
	Origin   string
	Now      func() time.Time
	MaxDrift time.Duration
}

// MetadataRecorder appends canonical metadata event artifacts.
type MetadataRecorder struct {
	mu       sync.Mutex
	database *db.DB
	root     string
	origin   string
	clock    *HLCClock
}

// NewMetadataRecorder creates a metadata event recorder for the local artifact store.
func NewMetadataRecorder(database *db.DB, opts MetadataRecorderOptions) *MetadataRecorder {
	root := ""
	if strings.TrimSpace(opts.DataDir) != "" {
		root = filepath.Join(opts.DataDir, "artifacts")
	}
	return &MetadataRecorder{
		database: database,
		root:     root,
		origin:   strings.TrimSpace(opts.Origin),
		clock: NewHLCClock(database, HLCClockOptions{
			Now:      opts.Now,
			MaxDrift: opts.MaxDrift,
		}),
	}
}

// WithMetadataEventSuppression marks a context as replaying metadata events.
func WithMetadataEventSuppression(ctx context.Context) context.Context {
	return context.WithValue(ctx, metadataSuppressionKey{}, true)
}

// MetadataEventsSuppressed reports whether local metadata event writes are disabled.
func MetadataEventsSuppressed(ctx context.Context) bool {
	suppressed, _ := ctx.Value(metadataSuppressionKey{}).(bool)
	return suppressed
}

// Append writes one metadata event artifact unless ctx is replay-suppressed.
func (r *MetadataRecorder) Append(ctx context.Context, input MetadataEventInput) (MetadataRecord, error) {
	if MetadataEventsSuppressed(ctx) {
		return MetadataRecord{}, nil
	}
	if r == nil {
		return MetadataRecord{}, nil
	}
	if r.database == nil {
		return MetadataRecord{}, errors.New("metadata recorder database is required")
	}
	if r.root == "" {
		return MetadataRecord{}, errors.New("metadata recorder data dir is required")
	}
	if input.SessionID == "" {
		return MetadataRecord{}, errors.New("metadata event session id is required")
	}
	if err := validateMetadataOp(input.Op); err != nil {
		return MetadataRecord{}, err
	}
	origin, err := r.resolveOrigin()
	if errors.Is(err, errOriginNotAdopted) {
		// The machine never opted into artifact sync: curation stays local.
		// If it joins a fleet later, the `sync --init` baseline snapshot
		// publishes the accumulated local curation state.
		return MetadataRecord{}, nil
	}
	if err != nil {
		return MetadataRecord{}, err
	}
	stamp, err := r.clock.Next()
	if err != nil {
		return MetadataRecord{}, err
	}
	event := metadataEvent{
		Version:    formatVersion,
		HLC:        stamp.String(),
		Origin:     origin,
		SessionGID: MetadataSessionGID(origin, input.SessionID),
		Op:         input.Op,
		Value:      input.Value,
		Pin:        input.Pin,
	}
	data, err := canonicalJSON(event)
	if err != nil {
		return MetadataRecord{}, err
	}
	hash := hashHex(data)
	orderKey := stamp.OrderingKey(hash)
	projection, err := metadataProjection(metadataArtifact{
		orderKey: orderKey,
		hash:     hash,
		hlc:      event.HLC,
		event:    event,
	}, origin)
	if err != nil {
		return MetadataRecord{}, err
	}
	path := filepath.Join(r.root, origin, "meta", orderKey+metadataEventExtension)
	record := MetadataRecord{
		HLC:        event.HLC,
		Origin:     origin,
		SessionGID: event.SessionGID,
		Op:         event.Op,
		Hash:       hash,
		Path:       path,
	}
	if err := writeFileAtomic(path, data, 0o644); err != nil {
		return MetadataRecord{}, fmt.Errorf("writing metadata event: %w", err)
	}
	// Record the local event in the LWW replay register only after the artifact
	// exists. Otherwise a failed publish can leave hidden local state that wins
	// future LWW comparisons for an event no peer can import.
	if _, err := r.database.RecordLocalMetadataProjection(ctx, projection); err != nil {
		return record, &MetadataPublishedError{
			Record: record,
			Err:    fmt.Errorf("recording local metadata replay state: %w", err),
		}
	}
	return record, nil
}

// RepairLocalSessionMetadata rebuilds local replay bookkeeping for already
// published local metadata artifacts without re-applying their visible
// mutations.
func (r *MetadataRecorder) RepairLocalSessionMetadata(
	ctx context.Context,
	sessionID string,
	ops ...string,
) (int, error) {
	if r == nil {
		return 0, nil
	}
	if r.database == nil {
		return 0, errors.New("metadata recorder database is required")
	}
	if r.root == "" {
		return 0, errors.New("metadata recorder data dir is required")
	}
	if sessionID == "" {
		return 0, errors.New("metadata event session id is required")
	}
	opSet := make(map[string]struct{}, len(ops))
	for _, op := range ops {
		if err := validateMetadataOp(op); err != nil {
			return 0, err
		}
		opSet[op] = struct{}{}
	}
	origin, err := r.resolveOrigin()
	if errors.Is(err, errOriginNotAdopted) {
		// No origin means no published local artifacts to repair against.
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	events, err := readMetadataArtifacts(filepath.Join(r.root, origin), origin, nil)
	if err != nil {
		return 0, err
	}
	sessionGID := MetadataSessionGID(origin, sessionID)
	repaired := 0
	for _, art := range events {
		if err := ctx.Err(); err != nil {
			return repaired, err
		}
		if art.event.SessionGID != sessionGID {
			continue
		}
		if len(opSet) > 0 {
			if _, ok := opSet[art.event.Op]; !ok {
				continue
			}
		}
		if err := validateMetadataArtifactEvent(art, origin); err != nil {
			if errors.Is(err, errFutureArtifactVersion) {
				continue
			}
			return repaired, err
		}
		if err := validateMetadataOp(art.event.Op); err != nil {
			return repaired, err
		}
		projection, err := metadataProjection(art, origin)
		if err != nil {
			return repaired, err
		}
		if _, err := r.database.RecordLocalMetadataProjection(ctx, projection); err != nil {
			return repaired, fmt.Errorf("repairing local metadata replay state: %w", err)
		}
		repaired++
	}
	return repaired, nil
}

// AppendBaseline writes metadata events for existing local curation that
// predates artifact metadata recording.
func (r *MetadataRecorder) AppendBaseline(ctx context.Context) (int, error) {
	if r == nil {
		return 0, nil
	}
	if r.database == nil {
		return 0, errors.New("metadata recorder database is required")
	}
	snap, err := r.database.MetadataBaselineSnapshot(ctx)
	if err != nil {
		return 0, err
	}
	return r.AppendBaselineSnapshot(ctx, snap)
}

// AppendBaselineSnapshot writes metadata events from a previously captured
// curation snapshot. Callers that import peer artifacts before initialization
// should capture the snapshot before that import so newly imported rows cannot
// be re-published as local baseline metadata.
func (r *MetadataRecorder) AppendBaselineSnapshot(
	ctx context.Context,
	snap db.MetadataBaselineSnapshot,
) (int, error) {
	if r == nil {
		return 0, nil
	}
	if r.database == nil {
		return 0, errors.New("metadata recorder database is required")
	}
	origin, err := r.resolveOrigin()
	if err != nil {
		return 0, err
	}
	written := 0
	for _, rename := range snap.Renames {
		covered, err := r.baselineFieldCovered(ctx, origin, rename.SessionID, "display_name")
		if err != nil {
			return written, err
		}
		if covered {
			continue
		}
		value, err := metadataRenameValue(rename.DisplayName)
		if err != nil {
			return written, err
		}
		if _, err := r.Append(ctx, MetadataEventInput{
			SessionID: rename.SessionID,
			Op:        MetadataOpRename,
			Value:     value,
		}); err != nil {
			return written, fmt.Errorf("writing baseline rename metadata: %w", err)
		}
		written++
	}
	for _, sessionID := range snap.StarredSessionIDs {
		covered, err := r.baselineFieldCovered(ctx, origin, sessionID, "starred")
		if err != nil {
			return written, err
		}
		if covered {
			continue
		}
		if _, err := r.Append(ctx, MetadataEventInput{
			SessionID: sessionID,
			Op:        MetadataOpStar,
		}); err != nil {
			return written, fmt.Errorf("writing baseline star metadata: %w", err)
		}
		written++
	}
	for _, sessionID := range snap.SoftDeletedIDs {
		covered, err := r.baselineFieldCovered(ctx, origin, sessionID, "deleted_at")
		if err != nil {
			return written, err
		}
		if covered {
			continue
		}
		if _, err := r.Append(ctx, MetadataEventInput{
			SessionID: sessionID,
			Op:        MetadataOpSoftDelete,
		}); err != nil {
			return written, fmt.Errorf("writing baseline soft-delete metadata: %w", err)
		}
		written++
	}
	for _, pin := range snap.Pins {
		metadataPin := MetadataPin{
			SourceUUID: pin.SourceUUID,
			Ordinal:    pin.Ordinal,
			Note:       pin.Note,
		}
		covered, err := r.baselineFieldCovered(
			ctx, origin, pin.SessionID, "pin:"+metadataPinAnchor(metadataPin),
		)
		if err != nil {
			return written, err
		}
		if covered {
			continue
		}
		if _, err := r.Append(ctx, MetadataEventInput{
			SessionID: pin.SessionID,
			Op:        MetadataOpPin,
			Pin:       &metadataPin,
		}); err != nil {
			return written, fmt.Errorf("writing baseline pin metadata: %w", err)
		}
		written++
	}
	return written, nil
}

func (r *MetadataRecorder) baselineFieldCovered(
	ctx context.Context,
	origin string,
	sessionID string,
	field string,
) (bool, error) {
	_, ok, err := r.database.MetadataReplayStateOp(
		ctx, MetadataSessionGID(origin, sessionID), field,
	)
	if err != nil {
		return false, fmt.Errorf("checking baseline metadata field %s: %w", field, err)
	}
	return ok, nil
}

// Import reads every foreign origin under root and imports referenced sessions
// plus metadata events, advancing this recorder's HLC clock past observed
// remote HLCs so later local edits stay causally ahead of imported peers.
func (r *MetadataRecorder) Import(ctx context.Context, root string) (ImportResult, error) {
	if r == nil || r.database == nil {
		return ImportResult{}, errors.New("metadata recorder database is required")
	}
	origin, err := r.resolveOrigin()
	if err != nil {
		return ImportResult{}, err
	}
	return importDetailed(ctx, r.database, r.clock, root, origin)
}

// MetadataSessionGID returns the global metadata target ID for a session.
func MetadataSessionGID(origin, sessionID string) string {
	if host, _ := parser.StripHostPrefix(sessionID); host != "" {
		return sessionID
	}
	return origin + "~" + sessionID
}

// resolveOrigin returns the recorder's origin without ever creating one: the
// explicit option wins, then the origin persisted in DB sync state. A machine
// with no origin anywhere has not opted into artifact sync and gets
// errOriginNotAdopted. The empty result is not cached, so a recorder built
// before opt-in starts resolving the origin as soon as it is adopted.
func (r *MetadataRecorder) resolveOrigin() (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.origin != "" {
		if err := validateOriginID(r.origin); err != nil {
			return "", fmt.Errorf("metadata recorder origin: %w", err)
		}
		return r.origin, nil
	}
	origin, err := StoredOrigin(r.database)
	if err != nil {
		return "", err
	}
	if origin == "" {
		return "", errOriginNotAdopted
	}
	r.origin = origin
	return origin, nil
}

func metadataRenameValue(displayName *string) (json.RawMessage, error) {
	data, err := json.Marshal(struct {
		DisplayName *string `json:"display_name"`
	}{DisplayName: displayName})
	if err != nil {
		return nil, err
	}
	return json.RawMessage(data), nil
}

func validateMetadataOp(op string) error {
	switch op {
	case MetadataOpRename,
		MetadataOpSoftDelete,
		MetadataOpRestore,
		MetadataOpStar,
		MetadataOpUnstar,
		MetadataOpPin,
		MetadataOpUnpin,
		MetadataOpPurge:
		return nil
	default:
		return fmt.Errorf("unsupported metadata event op %q", op)
	}
}
