//go:build windows

package clientd

import (
	"fmt"

	"golang.org/x/sys/windows"
)

func localRoots() []localEntry {
	mask, err := windows.GetLogicalDrives()
	if err != nil {
		return nil
	}
	roots := make([]localEntry, 0, 4)
	for i := 0; i < 26; i++ {
		if mask&(1<<i) == 0 {
			continue
		}
		letter := byte('A' + i)
		path := fmt.Sprintf("%c:\\", letter)
		roots = append(roots, localEntry{
			Name: fmt.Sprintf("本地磁盘 (%c:)", letter), Path: path, Dir: true,
		})
	}
	return roots
}
