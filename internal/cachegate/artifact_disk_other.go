//go:build !linux && !darwin

package cachegate

func artifactFreeSpace(string) (int64, error) { return 0, errArtifactSpace }

func sameArtifactFilesystem(string, string) bool { return false }
