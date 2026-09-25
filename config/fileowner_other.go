//go:build !unix

package config

// fileOwnerOf 在没有 POSIX 属主概念的平台上不存在恢复属主的问题。
func fileOwnerOf(path string) (uid, gid int, ok bool) {
	return 0, 0, false
}
