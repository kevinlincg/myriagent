package agent

import "golang.org/x/sys/unix"

// folderOnNetworkFS：資料夾在網路磁碟上（NFS、SMB／CIFS）
func folderOnNetworkFS(path string) bool {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return false
	}
	switch uint32(st.Type) {
	case 0x6969, // NFS
		0x517B,     // SMB
		0xFF534D42, // CIFS
		0xFE534D42: // SMB2
		return true
	}
	return false
}
