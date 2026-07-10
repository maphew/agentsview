package artifact

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// GCOptions configures conservative artifact garbage collection.
type GCOptions struct {
	Root   string
	Grace  time.Duration
	DryRun bool
	Now    time.Time
	Logf   func(string, ...any)
}

// GCResult summarizes one artifact garbage collection scan.
type GCResult struct {
	DryRun         bool
	Origins        int
	SkippedOrigins int
	Scanned        int
	Candidates     int
	Eligible       int
	KeptByGrace    int
	Deleted        int
	BytesEligible  int64
	BytesDeleted   int64
}

type gcRef struct {
	kind string
	name string
}

type gcCandidate struct {
	path    string
	origin  string
	kind    string
	name    string
	size    int64
	modTime time.Time
}

type gcOriginResult struct {
	skipped    bool
	scanned    int
	candidates []gcCandidate
	kindRoots  map[string]*os.Root
}

// GarbageCollect deletes or reports superseded artifacts that are no longer
// reachable from an origin's latest checkpoint and its live manifests.
func GarbageCollect(ctx context.Context, opts GCOptions) (GCResult, error) {
	if opts.Root == "" {
		return GCResult{}, fmt.Errorf("artifact gc root is required")
	}
	if opts.Grace < 0 {
		return GCResult{}, fmt.Errorf("artifact gc grace must be >= 0")
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	cutoff := now.Add(-opts.Grace)

	storeRoot, err := openArtifactRoot(opts.Root, "gc")
	if err != nil {
		return GCResult{}, fmt.Errorf("opening artifact gc root: %w", err)
	}
	defer storeRoot.Close()
	origins, err := listGCOrigins(storeRoot)
	if err != nil {
		return GCResult{}, fmt.Errorf("listing artifact origins: %w", err)
	}
	res := GCResult{DryRun: opts.DryRun}
	for _, origin := range origins {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		originRoot, err := openArtifactSubroot(storeRoot, origin, "gc origin")
		if err != nil {
			return res, fmt.Errorf("opening artifact gc origin %s: %w", origin, err)
		}
		originRes, err := collectGCCandidates(ctx, originRoot, origin)
		if err != nil {
			_ = originRoot.Close()
			if errors.Is(err, errIncompleteArtifact) ||
				errors.Is(err, errCorruptArtifact) ||
				errors.Is(err, errFutureArtifactVersion) {
				res.Origins++
				res.SkippedOrigins++
				logGC(opts, "artifact gc: skipping %s: %v", origin, err)
				continue
			}
			return res, fmt.Errorf("scanning %s: %w", origin, err)
		}
		res.Origins++
		if originRes.skipped {
			closeGCKindRoots(originRes.kindRoots)
			_ = originRoot.Close()
			res.SkippedOrigins++
			logGC(opts, "artifact gc: skipping %s with no checkpoints", origin)
			continue
		}
		res.Scanned += originRes.scanned
		for _, cand := range originRes.candidates {
			res.Candidates++
			if cand.modTime.After(cutoff) {
				res.KeptByGrace++
				continue
			}
			res.Eligible++
			res.BytesEligible += cand.size
			if opts.DryRun {
				logGC(opts, "artifact gc: would delete %s (%d bytes)", cand.path, cand.size)
				continue
			}
			if err := originRes.kindRoots[cand.kind].Remove(cand.name); err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					continue
				}
				closeGCKindRoots(originRes.kindRoots)
				_ = originRoot.Close()
				return res, fmt.Errorf("deleting %s: %w", cand.path, err)
			}
			res.Deleted++
			res.BytesDeleted += cand.size
			logGC(opts, "artifact gc: deleted %s (%d bytes)", cand.path, cand.size)
		}
		closeGCKindRoots(originRes.kindRoots)
		if err := originRoot.Close(); err != nil {
			return res, fmt.Errorf("closing artifact gc origin %s: %w", origin, err)
		}
	}
	return res, nil
}

func listGCOrigins(root *os.Root) ([]string, error) {
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return nil, err
	}
	origins := make([]string, 0, len(entries))
	for _, ent := range entries {
		if validateOriginID(ent.Name()) != nil {
			continue
		}
		info, err := root.Lstat(ent.Name())
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("artifact origin %s is not a directory", ent.Name())
		}
		origins = append(origins, ent.Name())
	}
	sort.Strings(origins)
	return origins, nil
}

func collectGCCandidates(ctx context.Context, root *os.Root, origin string) (gcOriginResult, error) {
	kindRoots, err := openGCKindRoots(root)
	if err != nil {
		return gcOriginResult{}, err
	}
	keepRoots := false
	defer func() {
		if !keepRoots {
			closeGCKindRoots(kindRoots)
		}
	}()
	checkpoints, err := validGCCheckpointNames(kindRoots[KindCheckpoints])
	if err != nil {
		return gcOriginResult{}, err
	}
	if len(checkpoints) == 0 {
		return gcOriginResult{skipped: true}, nil
	}

	latestName := checkpoints[len(checkpoints)-1]
	data, err := kindRoots[KindCheckpoints].ReadFile(latestName)
	if err != nil {
		return gcOriginResult{}, fmt.Errorf("reading live checkpoint: %w", err)
	}
	var cp checkpoint
	if err := json.Unmarshal(data, &cp); err != nil {
		return gcOriginResult{}, fmt.Errorf("%w: decoding live checkpoint: %v", errCorruptArtifact, err)
	}
	if err := validateCheckpoint(&cp, origin); err != nil {
		if !errors.Is(err, errFutureArtifactVersion) {
			err = fmt.Errorf("%w: validating live checkpoint: %v", errCorruptArtifact, err)
		}
		return gcOriginResult{}, err
	}
	if err := validateCheckpointSequenceIdentity(cp, latestName); err != nil {
		return gcOriginResult{}, fmt.Errorf(
			"%w: validating live checkpoint identity: %v", errCorruptArtifact, err,
		)
	}
	live := map[gcRef]struct{}{
		{kind: KindCheckpoints, name: latestName}: {},
	}

	gids := make([]string, 0, len(cp.Sessions))
	for gid := range cp.Sessions {
		gids = append(gids, gid)
	}
	sort.Strings(gids)
	for _, gid := range gids {
		if err := ctx.Err(); err != nil {
			return gcOriginResult{}, err
		}
		manifestHash := cp.Sessions[gid]
		if err := validateHashHex(manifestHash); err != nil {
			return gcOriginResult{}, fmt.Errorf("live manifest %s: %w", gid, err)
		}
		manifestName := manifestHash + manifestExtension
		live[gcRef{kind: KindManifests, name: manifestName}] = struct{}{}

		m, err := readGCManifest(kindRoots[KindManifests], manifestHash)
		if err != nil {
			return gcOriginResult{}, fmt.Errorf("reading live manifest %s: %w", manifestHash, err)
		}
		if err := validateManifest(m, origin, gid); err != nil {
			if !errors.Is(err, errFutureArtifactVersion) {
				err = fmt.Errorf("%w: validating live manifest: %v", errCorruptArtifact, err)
			}
			return gcOriginResult{}, err
		}
		if err := validateGCManifestMessagesWithLimits(
			kindRoots[KindSegments], m, productionArtifactLimits(),
		); err != nil {
			return gcOriginResult{}, fmt.Errorf("reading live segments for %s: %w", gid, err)
		}
		for _, segmentHash := range m.Segments {
			live[gcRef{kind: KindSegments, name: segmentHash + segmentExtension}] = struct{}{}
		}
		if m.RawSource != nil && m.RawSource.Hash != "" {
			if err := validateLiveRawSource(kindRoots[KindRaw], *m.RawSource); err != nil {
				return gcOriginResult{}, fmt.Errorf("reading live raw source for %s: %w", gid, err)
			}
			live[gcRef{kind: KindRaw, name: m.RawSource.Hash}] = struct{}{}
		}
	}

	candidates, scanned, err := scanGCCandidates(ctx, root, kindRoots, origin, live)
	if err != nil {
		return gcOriginResult{}, err
	}
	keepRoots = true
	return gcOriginResult{
		scanned: scanned, candidates: candidates, kindRoots: kindRoots,
	}, nil
}

func openGCKindRoots(root *os.Root) (map[string]*os.Root, error) {
	roots := make(map[string]*os.Root, 4)
	for _, kind := range []string{KindCheckpoints, KindManifests, KindSegments, KindRaw} {
		info, err := root.Lstat(kind)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			closeGCKindRoots(roots)
			return nil, err
		}
		if !info.IsDir() {
			closeGCKindRoots(roots)
			return nil, fmt.Errorf("artifact kind %s is not a directory", kind)
		}
		kindRoot, err := openArtifactSubroot(root, kind, "gc kind")
		if err != nil {
			closeGCKindRoots(roots)
			return nil, err
		}
		roots[kind] = kindRoot
	}
	return roots, nil
}

func closeGCKindRoots(roots map[string]*os.Root) {
	for _, root := range roots {
		_ = root.Close()
	}
}

func readGCManifest(root *os.Root, hash string) (manifest, error) {
	if root == nil {
		return manifest{}, fmt.Errorf("%w: manifest %s", errIncompleteArtifact, hash)
	}
	name := hash + manifestExtension
	compressed, err := root.ReadFile(name)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return manifest{}, fmt.Errorf("%w: manifest %s", errIncompleteArtifact, hash)
		}
		return manifest{}, err
	}
	data, err := readCompressedBytes(compressed, manifestDecodedLimit)
	if err != nil {
		quarantineArtifactRoot(root, name)
		return manifest{}, fmt.Errorf("%w: decoding manifest %s: %v", errCorruptArtifact, hash, err)
	}
	if got := hashHex(data); got != hash {
		quarantineArtifactRoot(root, name)
		return manifest{}, fmt.Errorf(
			"%w: manifest %s hash mismatch: got %s", errCorruptArtifact, hash, got,
		)
	}
	m, err := decodeManifestWithLimits(data, productionArtifactLimits())
	if err != nil {
		quarantineArtifactRoot(root, name)
		return manifest{}, fmt.Errorf("%w: decoding manifest %s: %v", errCorruptArtifact, hash, err)
	}
	return m, nil
}

func validateGCManifestMessagesWithLimits(
	root *os.Root, m manifest, limits artifactLimits,
) error {
	if err := validateManifestReferencesWithLimits(m, limits); err != nil {
		return fmt.Errorf("%w: %v", errCorruptArtifact, err)
	}
	if root == nil && len(m.Segments) > 0 {
		return fmt.Errorf("%w: segment %s", errIncompleteArtifact, m.Segments[0])
	}
	var decodedBytes int64
	totalMessages := 0
	totalNested := nestedCollectionCounts{}
	for _, hash := range m.Segments {
		name := hash + segmentExtension
		compressed, err := root.ReadFile(name)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("%w: segment %s", errIncompleteArtifact, hash)
			}
			return err
		}
		data, err := readCompressedBytes(compressed, segmentDecodedLimit)
		if err != nil {
			quarantineArtifactRoot(root, name)
			return fmt.Errorf("%w: segment %s: %v", errCorruptArtifact, hash, err)
		}
		if got := hashHex(data); got != hash {
			quarantineArtifactRoot(root, name)
			return fmt.Errorf(
				"%w: segment %s hash mismatch: got %s", errCorruptArtifact, hash, got,
			)
		}
		preflight, err := preflightSegmentData(data, limits)
		if err != nil {
			if errors.Is(err, errFutureArtifactVersion) {
				return fmt.Errorf("segment %s: %w", hash, err)
			}
			quarantineArtifactRoot(root, name)
			return fmt.Errorf("%w: segment %s: %v", errCorruptArtifact, hash, err)
		}
		segmentBytes := int64(len(data))
		if segmentBytes > limits.sessionDecodedBytes-decodedBytes {
			return fmt.Errorf(
				"%w: session decoded byte limit exceeded: limit %d",
				errCorruptArtifact, limits.sessionDecodedBytes,
			)
		}
		if len(preflight.records) > limits.sessionMessages-totalMessages {
			return fmt.Errorf(
				"%w: session message limit exceeded: limit %d",
				errCorruptArtifact, limits.sessionMessages,
			)
		}
		if exceedsCollectionLimit(
			totalNested.toolCalls, preflight.nested.toolCalls, limits.sessionToolCalls,
		) {
			return fmt.Errorf(
				"%w: session tool call limit exceeded: limit %d",
				errCorruptArtifact, limits.sessionToolCalls,
			)
		}
		if exceedsCollectionLimit(
			totalNested.resultEvents,
			preflight.nested.resultEvents,
			limits.sessionResultEvents,
		) {
			return fmt.Errorf(
				"%w: session result event limit exceeded: limit %d",
				errCorruptArtifact, limits.sessionResultEvents,
			)
		}
		messages, err := decodePreflightedSegment(preflight)
		if err != nil {
			quarantineArtifactRoot(root, name)
			return fmt.Errorf("%w: segment %s: %v", errCorruptArtifact, hash, err)
		}
		decodedBytes += segmentBytes
		totalMessages += len(messages)
		totalNested.toolCalls += preflight.nested.toolCalls
		totalNested.resultEvents += preflight.nested.resultEvents
	}
	return nil
}

func validateLiveRawSource(root *os.Root, ref rawSourceRef) error {
	if err := validateHashHex(ref.Hash); err != nil {
		return err
	}
	if root == nil {
		return fmt.Errorf("%w: raw source %s", errIncompleteArtifact, ref.Hash)
	}
	info, err := root.Lstat(ref.Hash)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%w: raw source %s", errIncompleteArtifact, ref.Hash)
		}
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%w: raw source %s is not a regular file", errCorruptArtifact, ref.Hash)
	}
	if ref.Size != 0 && info.Size() != ref.Size {
		return fmt.Errorf(
			"%w: raw source %s size mismatch: got %d, want %d",
			errCorruptArtifact, ref.Hash, info.Size(), ref.Size,
		)
	}

	file, err := root.Open(ref.Hash)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%w: raw source %s", errIncompleteArtifact, ref.Hash)
		}
		return err
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil {
		return err
	}
	if !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) {
		return fmt.Errorf("%w: raw source %s changed while opening", errCorruptArtifact, ref.Hash)
	}
	h := sha256.New()
	size, err := io.Copy(h, file)
	if err != nil {
		return err
	}
	if ref.Size != 0 && size != ref.Size {
		return fmt.Errorf(
			"%w: raw source %s size mismatch while reading: got %d, want %d",
			errCorruptArtifact, ref.Hash, size, ref.Size,
		)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != ref.Hash {
		return fmt.Errorf(
			"%w: raw source %s hash mismatch: got %s",
			errCorruptArtifact, ref.Hash, got,
		)
	}
	return nil
}

func validGCCheckpointNames(root *os.Root) ([]string, error) {
	if root == nil {
		return nil, nil
	}
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return nil, err
	}
	valid := make([]string, 0, len(entries))
	for _, ent := range entries {
		if _, err := checkpointSequence(ent.Name()); err == nil {
			valid = append(valid, ent.Name())
		}
	}
	sort.Strings(valid)
	return valid, nil
}

func scanGCCandidates(
	ctx context.Context,
	originRoot *os.Root,
	kindRoots map[string]*os.Root,
	origin string,
	live map[gcRef]struct{},
) ([]gcCandidate, int, error) {
	var candidates []gcCandidate
	scanned := 0
	for _, spec := range []struct {
		kind  string
		valid func(string) bool
	}{
		{kind: KindCheckpoints, valid: isGCCheckpointName},
		{kind: KindManifests, valid: isGCManifestName},
		{kind: KindSegments, valid: isGCSegmentName},
		{kind: KindRaw, valid: isGCRawName},
	} {
		dir := filepath.Join(originRoot.Name(), spec.kind)
		kindRoot := kindRoots[spec.kind]
		if kindRoot == nil {
			continue
		}
		entries, err := fs.ReadDir(kindRoot.FS(), ".")
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, scanned, fmt.Errorf("reading %s: %w", dir, err)
		}
		for _, ent := range entries {
			if err := ctx.Err(); err != nil {
				return nil, scanned, err
			}
			if ent.IsDir() || isTempArtifactEntry(ent.Name()) || !spec.valid(ent.Name()) {
				continue
			}
			info, err := ent.Info()
			if err != nil {
				return nil, scanned, fmt.Errorf("stat %s: %w", filepath.Join(dir, ent.Name()), err)
			}
			if !info.Mode().IsRegular() {
				continue
			}
			scanned++
			if _, ok := live[gcRef{kind: spec.kind, name: ent.Name()}]; ok {
				continue
			}
			candidates = append(candidates, gcCandidate{
				path:    filepath.Join(dir, ent.Name()),
				origin:  origin,
				kind:    spec.kind,
				name:    ent.Name(),
				size:    info.Size(),
				modTime: info.ModTime(),
			})
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].origin != candidates[j].origin {
			return candidates[i].origin < candidates[j].origin
		}
		if candidates[i].kind != candidates[j].kind {
			return candidates[i].kind < candidates[j].kind
		}
		return candidates[i].name < candidates[j].name
	})
	return candidates, scanned, nil
}

func isGCCheckpointName(name string) bool {
	_, err := checkpointSequence(name)
	return err == nil
}

func isGCManifestName(name string) bool {
	_, _, err := normalizeHashName(name, manifestExtension)
	return err == nil
}

func isGCSegmentName(name string) bool {
	_, _, err := normalizeHashName(name, segmentExtension)
	return err == nil
}

func isGCRawName(name string) bool {
	return validateHashHex(name) == nil
}

func logGC(opts GCOptions, format string, args ...any) {
	if opts.Logf != nil {
		opts.Logf(format, args...)
	}
}
