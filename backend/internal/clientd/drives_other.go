//go:build !windows

package clientd

func localRoots() []localEntry {
	return []localEntry{{Name: "文件系统", Path: "/", Dir: true}}
}
