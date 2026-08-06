package engine

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/zeebo/blake3"

	"github.com/guru-bharadwaj20/private-cloud/client/internal/api"
	"github.com/guru-bharadwaj20/private-cloud/client/internal/chunk"
)

// uploadFile sends a local file to the server and returns the resulting node.
//
// Below the chunking threshold a file is sent whole — a manifest to describe a
// few hundred bytes is pure overhead, and the server stores it as a blob. At or
// above the threshold the delta path runs: chunk locally, ask which chunks the
// server lacks, upload only those, and commit a manifest. That is where "a 4 GB
// file that changed by one block moves one block" actually happens.
func (e *Engine) uploadFile(ctx context.Context, serverPath string, parentID string) (node api.Node, localHash string, err error) {
	local := e.localPath(serverPath)
	info, err := os.Stat(local)
	if err != nil {
		return api.Node{}, "", err
	}
	name := baseName(serverPath)

	if info.Size() < chunk.WholeFileThreshold {
		return e.uploadWhole(ctx, local, parentID, name)
	}
	return e.uploadChunked(ctx, local, parentID, name)
}

// uploadWhole reads a small file into memory (it is below the chunk threshold, so
// at most a couple of kilobytes), sends it raw, and returns the node along with
// the client's whole-file BLAKE3 for the state record.
func (e *Engine) uploadWhole(ctx context.Context, local, parentID, name string) (api.Node, string, error) {
	data, err := os.ReadFile(local)
	if err != nil {
		return api.Node{}, "", err
	}
	sum := blake3.Sum256(data)
	node, err := e.srv.Upload(ctx, parentID, name, bytes.NewReader(data))
	return node, hex.EncodeToString(sum[:]), err
}

// uploadChunked runs the delta upload. It reads the file twice: once to learn the
// chunk hashes without holding any plaintext, then — after negotiating which are
// missing — again to upload only those. Two cheap sequential reads cost far less
// than pinning a whole large file in memory.
func (e *Engine) uploadChunked(ctx context.Context, local, parentID, name string) (api.Node, string, error) {
	order, whole, err := hashChunks(ctx, local)
	if err != nil {
		return api.Node{}, "", err
	}

	// Deduplicate before asking: a file with repeated blocks should query each
	// distinct hash once.
	uniq := dedupe(order)
	missing, err := e.srv.HaveChunks(ctx, uniq)
	if err != nil {
		return api.Node{}, "", err
	}
	need := make(map[string]bool, len(missing))
	for _, h := range missing {
		need[h] = true
	}

	if len(need) > 0 {
		f, err := os.Open(local)
		if err != nil {
			return api.Node{}, "", err
		}
		defer f.Close()
		uploaded := make(map[string]bool, len(need))
		_, err = chunk.Split(ctx, f, func(c *chunk.Chunk) error {
			h := hex.EncodeToString(c.Hash[:])
			if !need[h] || uploaded[h] {
				return nil
			}
			if err := e.srv.PutChunk(ctx, h, c.Plain); err != nil {
				return fmt.Errorf("upload chunk %s: %w", h, err)
			}
			uploaded[h] = true
			return nil
		})
		if err != nil {
			return api.Node{}, "", err
		}
	}

	node, err := e.srv.CommitManifest(ctx, parentID, name, whole, order, "")
	return node, whole, err
}

// hashChunks reads a file and returns its ordered chunk hashes and whole-file
// hash, holding no plaintext — the cheap first pass of a delta upload.
func hashChunks(ctx context.Context, local string) (order []string, whole string, err error) {
	f, err := os.Open(local)
	if err != nil {
		return nil, "", err
	}
	defer f.Close()
	w, err := chunk.Split(ctx, f, func(c *chunk.Chunk) error {
		order = append(order, hex.EncodeToString(c.Hash[:]))
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	return order, hex.EncodeToString(w[:]), nil
}

func dedupe(hashes []string) []string {
	seen := make(map[string]bool, len(hashes))
	out := make([]string, 0, len(hashes))
	for _, h := range hashes {
		if !seen[h] {
			seen[h] = true
			out = append(out, h)
		}
	}
	return out
}

// downloadFile materializes a node's content locally and returns the written
// file's whole-file BLAKE3, size and mtime for the state record.
//
// The write goes to a temp file in the state directory and is renamed into place,
// so a crash or a concurrent reader never sees a half-written file — the file
// either has its old content or its new content, never a torn mixture.
func (e *Engine) downloadFile(ctx context.Context, node api.Node, serverPath string) (hash string, size int64, mtime int64, err error) {
	dest := e.localPath(serverPath)
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", 0, 0, err
	}

	tmp, err := e.tempFile()
	if err != nil {
		return "", 0, 0, err
	}
	tmpName := tmp.Name()
	// If anything below fails, do not leave the temp file behind.
	defer func() {
		if err != nil {
			tmp.Close()
			os.Remove(tmpName)
		}
	}()

	hasher := blake3.New()
	sink := io.MultiWriter(tmp, hasher)

	man, err := e.srv.Manifest(ctx, node.ID)
	if err != nil {
		return "", 0, 0, err
	}
	if man.Kind == "chunked" {
		for _, ce := range man.Chunks {
			plain, gerr := e.srv.GetChunk(ctx, ce.Hash)
			if gerr != nil {
				return "", 0, 0, gerr
			}
			if _, werr := sink.Write(plain); werr != nil {
				return "", 0, 0, werr
			}
		}
	} else {
		rc, derr := e.srv.Download(ctx, node.ID)
		if derr != nil {
			return "", 0, 0, derr
		}
		_, werr := io.Copy(sink, rc)
		rc.Close()
		if werr != nil {
			return "", 0, 0, werr
		}
	}

	if err = tmp.Sync(); err != nil {
		return "", 0, 0, err
	}
	if err = tmp.Close(); err != nil {
		return "", 0, 0, err
	}
	if err = os.Rename(tmpName, dest); err != nil {
		return "", 0, 0, err
	}

	info, err := os.Stat(dest)
	if err != nil {
		return "", 0, 0, err
	}
	var sum [32]byte
	copy(sum[:], hasher.Sum(nil))
	return hex.EncodeToString(sum[:]), info.Size(), info.ModTime().Unix(), nil
}

// tempFile creates a download scratch file in the state directory, which is never
// part of the synced tree and shares a filesystem with the root so the rename
// into place is atomic.
func (e *Engine) tempFile() (*os.File, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, err
	}
	return os.OpenFile(
		filepath.Join(e.stateDir, "download-"+hex.EncodeToString(b[:])+".tmp"),
		os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
}
