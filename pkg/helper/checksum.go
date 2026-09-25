package helper

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// VerifyChecksum checks target against the manifest's SHA256 entry for asset.
// The panel (before staging) and the helper (before installing) run the same
// verification, so the release checksum manifest is honored at every step of
// an update.
func VerifyChecksum(manifest, asset, target string) error {
	data, err := os.ReadFile(manifest)
	if err != nil {
		return err
	}
	var expected string
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) == 2 && fields[1] == asset {
			expected = strings.ToLower(fields[0])
		}
	}
	if len(expected) != 64 {
		return errors.New("manifest has no entry for " + asset)
	}
	f, err := os.Open(target)
	if err != nil {
		return err
	}
	defer f.Close()
	sum := sha256.New()
	if _, err = io.Copy(sum, f); err != nil {
		return err
	}
	if got := hex.EncodeToString(sum.Sum(nil)); got != expected {
		return fmt.Errorf("checksum mismatch: expected %s, got %s", expected, got)
	}
	return nil
}
