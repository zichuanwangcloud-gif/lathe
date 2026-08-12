// Package migrations 以 embed.FS 形式携带 Lathe 的 SQL 迁移脚本。
//
// 迁移随二进制一起分发 —— 部署到新节点只需拷贝一个文件，
// 不需要额外带上 migrations 目录（见 docs/03-tech-stack.md §3 理由②）。
package migrations

import "embed"

// FS 包含本目录下全部 .sql 迁移脚本。
//
//go:embed *.sql
var FS embed.FS
