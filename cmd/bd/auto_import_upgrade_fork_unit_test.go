package main

import "context"

// GetAllConfig gives upstream's single-transaction fake store the config read
// the fork-only memory guard makes before ImportJSONLData (countMemoriesInStore
// in auto_import_upgrade.go). The fake embeds a nil storage.DoltStorage, so
// without this the guard would panic; an empty config holds no memories, so the
// guard lets the import run and upstream's assertions see the same path.
func (f *fakeJSONLImporterStore) GetAllConfig(context.Context) (map[string]string, error) {
	return map[string]string{}, nil
}
