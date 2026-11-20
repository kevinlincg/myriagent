package agent

import (
	"strings"

	"golang.org/x/sys/unix"
)

// folderOnNetworkFS：資料夾在網路磁碟上（SMB、NFS、AFP、WebDAV）。讀不到（資料夾不在）當成不是
func folderOnNetworkFS(path string) bool {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return false
	}
	name := unix.ByteSliceToString(st.Fstypename[:])
	switch strings.ToLower(name) {
	case "smbfs", "nfs", "afpfs", "webdav", "cifs", "ftp":
		return true
	}
	return false
}
