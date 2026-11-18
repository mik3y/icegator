package config

import (
	"errors"
	"io/fs"
	"path/filepath"

	"github.com/joho/godotenv"
)

// EnvFiles are the files LoadEnv looks for beside the config file, in the
// order they are read. A variable set in an earlier one is not changed by a
// later one, so .env.local is where a machine's own values go.
var EnvFiles = []string{".env.local", ".env"}

// LoadEnv reads variables from .env files into the environment, so that a
// config file can refer to them as ${NAME} without anyone having to export
// them first. It returns the files it read.
//
// With explicit empty, it reads whichever of EnvFiles exist in the same
// directory as the config file, and it is not an error for none to. With
// explicit set, it reads that one file, which must exist.
//
// A variable that is already set in the environment is never overwritten,
// so what the process was started with always wins.
func LoadEnv(configPath, explicit string) ([]string, error) {
	if explicit != "" {
		return []string{explicit}, godotenv.Load(explicit)
	}
	var read []string
	for _, name := range EnvFiles {
		path := filepath.Join(filepath.Dir(configPath), name)
		err := godotenv.Load(path)
		switch {
		case err == nil:
			read = append(read, path)
		case errors.Is(err, fs.ErrNotExist):
		default:
			return read, err
		}
	}
	return read, nil
}
