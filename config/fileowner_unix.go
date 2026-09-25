//go:build unix

package config

import (
	"os"
	"syscall"
)

// fileOwnerOf 返回文件的 uid/gid 属主；平台或文件系统不支持时 ok=false。
// 迁移用它在原子替换后恢复原属主——helper 以 root 执行迁移，临时文件默认
// 归 root:root，直接 rename 会把 config.toml 的属主从 root:lightpanel 改掉，
// 使依赖组权限读配置的面板用户失去读权限。
func fileOwnerOf(path string) (uid, gid int, ok bool) {
	st, err := os.Stat(path)
	if err != nil {
		return 0, 0, false
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return int(sys.Uid), int(sys.Gid), true
}
