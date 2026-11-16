package credhelper

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// HelperName is the value written to credHelpers; Docker runs docker-credential-registrygate for it.
const HelperName = "registrygate"

// ConfigureFile sets credHelpers[host] = "registrygate" for every host in the Docker config file at path, keeping
// every other key. It creates the file (0600) and its directory (0700) when missing. A file that is not a JSON
// object, or a credHelpers value that is not an object, is left untouched and reported as an error.
func ConfigureFile(path string, hosts []string) error {
	if len(hosts) == 0 {
		return errors.New("no hosts listed: set " + HostsEnv)
	}

	config := map[string]json.RawMessage{}

	raw, err := os.ReadFile(path)

	switch {
	case err == nil:
		if err := json.Unmarshal(raw, &config); err != nil || config == nil {
			return fmt.Errorf("%s is not a JSON object; leaving it untouched", path)
		}
	case !errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("read %s: %w", path, err)
	}

	helpers := map[string]json.RawMessage{}

	if existing, ok := config["credHelpers"]; ok {
		if err := json.Unmarshal(existing, &helpers); err != nil || helpers == nil {
			return fmt.Errorf("credHelpers in %s is not a JSON object; leaving it untouched", path)
		}
	}

	name, _ := json.Marshal(HelperName)
	for _, host := range hosts {
		helpers[host] = name
	}

	encoded, err := json.Marshal(helpers)
	if err != nil {
		return err
	}

	config["credHelpers"] = encoded

	var out bytes.Buffer

	enc := json.NewEncoder(&out)
	enc.SetIndent("", "  ")

	if err := enc.Encode(config); err != nil {
		return err
	}

	return writeAtomic(path, out.Bytes())
}

// writeAtomic writes data to a temp file beside path and renames it over path.
func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, ".config.json-*")
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}

	defer func() { _ = os.Remove(tmp.Name()) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()

		return fmt.Errorf("write %s: %w", path, err)
	}

	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}

	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}

	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}

	return nil
}
