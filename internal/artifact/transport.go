package artifact

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
)

// Transport exchanges immutable, content-addressed artifacts between the local
// store and a remote target using set-union semantics: publish local-only
// artifacts to the remote and fetch remote-only artifacts into the local store.
// Because every artifact is write-once and content-addressed, exchange is
// order-independent and idempotent, so folder, HTTP peer, and object-store
// targets are interchangeable behind this interface.
type Transport interface {
	// Prepare validates the target against the local store and creates any
	// remote-side structure required before exchange. It runs before the local
	// export so a misconfigured target fails fast.
	Prepare(ctx context.Context, localRoot string) error
	// Exchange performs the set-union publish (local-only -> remote) and fetch
	// (remote-only -> local).
	Exchange(ctx context.Context, localRoot string) error
}

// folderTransport exchanges artifacts with a local filesystem folder: a synced
// share such as Syncthing, Dropbox, NFS, or an rclone mount.
type folderTransport struct {
	target string
}

func (t *folderTransport) Prepare(_ context.Context, localRoot string) error {
	if err := validateDisjointRoots(localRoot, t.target); err != nil {
		return err
	}
	if err := os.MkdirAll(t.target, 0o755); err != nil {
		return fmt.Errorf("creating artifact sync target: %w", err)
	}
	return nil
}

func (t *folderTransport) Exchange(_ context.Context, localRoot string) error {
	if err := CopyUnion(localRoot, t.target); err != nil {
		return fmt.Errorf("publishing artifacts: %w", err)
	}
	if err := CopyUnion(t.target, localRoot); err != nil {
		return fmt.Errorf("fetching artifacts: %w", err)
	}
	return nil
}

// reconcileCommonCheckpoint guards the HTTP and S3 exchanges against silent
// checkpoint divergence: checkpoints are sequence-named rather than
// content-addressed, so a rebuilt store or an accidentally shared origin id
// can hold different bytes under the same name, which name-set comparison
// alone would skip forever. The highest common checkpoint name is compared by
// content each exchange: a corrupt local copy is quarantined (the caller
// refreshes its index so the remote copy is re-fetched), a corrupt remote
// copy is left for its owner to repair, and a valid-but-divergent pair fails
// loudly with the same conflict error the folder transport raises. Divergence
// buried below the highest common sequence is not scanned; import only trusts
// the latest compatible checkpoint, so it cannot change the imported state.
func reconcileCommonCheckpoint(
	localRoot, origin string,
	local, remote OriginArtifactIndex,
	fetch func(name string) ([]byte, error),
) (quarantined bool, err error) {
	name := highestCommonCheckpoint(local, remote)
	if name == "" {
		return false, nil
	}
	remoteData, err := fetch(name)
	if err != nil {
		if errors.Is(err, ErrArtifactNotFound) {
			// The remote no longer serves its copy, typically after
			// quarantining a corrupt one; nothing to compare this round.
			return false, nil
		}
		return false, err
	}
	art, err := ReadArtifact(localRoot, origin, KindCheckpoints, name)
	if err != nil {
		if errors.Is(err, ErrArtifactInvalid) {
			log.Printf("artifact: quarantining corrupt local checkpoint %s/%s: %v", origin, name, err)
			quarantineArtifact(filepath.Join(localRoot, origin, KindCheckpoints, name))
			return true, nil
		}
		if errors.Is(err, ErrArtifactNotFound) {
			return false, nil
		}
		return false, err
	}
	if bytes.Equal(art.Data, remoteData) {
		return false, nil
	}
	spec, err := artifactSpecForKind(origin, KindCheckpoints, name)
	if err != nil {
		return false, err
	}
	if verr := validateArtifactData(spec, remoteData); verr != nil {
		log.Printf("artifact: remote holds corrupt checkpoint %s/%s: %v", origin, name, verr)
		return false, nil
	}
	return false, fmt.Errorf(
		"%w: checkpoint %s/%s differs between the local store and the remote; "+
			"was this origin's artifact store rebuilt or its origin id reused?",
		errArtifactPathConflict, origin, name,
	)
}

func highestCommonCheckpoint(local, remote OriginArtifactIndex) string {
	names := make(map[string]struct{}, len(local.Checkpoints))
	for _, name := range local.Checkpoints {
		names[name] = struct{}{}
	}
	best := ""
	for _, name := range remote.Checkpoints {
		if _, ok := names[name]; ok && name > best {
			best = name
		}
	}
	return best
}
