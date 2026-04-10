package static

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"sync"
)

// AssetVersion returns a short hash of all embedded assets, used as a
// cache-busting query string (e.g. /static/dist/bundle.js?v=abc1234).
// Computed once on first call, lazily.
func AssetVersion() string {
	assetVersionOnce.Do(func() {
		assetVersionValue = computeAssetVersion()
	})

	return assetVersionValue
}

//nolint:gochecknoglobals // memoized once via sync.Once
var (
	assetVersionOnce  sync.Once
	assetVersionValue string
)

func computeAssetVersion() string {
	hasher := sha256.New()

	walkErr := fs.WalkDir(FS, ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if entry.IsDir() {
			return nil
		}

		data, readErr := fs.ReadFile(FS, path)
		if readErr != nil {
			return readErr //nolint:wrapcheck // internal walk
		}

		_, _ = hasher.Write([]byte(path))
		_, _ = hasher.Write(data)

		return nil
	})
	if walkErr != nil {
		return "dev"
	}

	return hex.EncodeToString(hasher.Sum(nil))[:8]
}
