// Package web 内嵌构建好的界面文件（web/ 目录下用 npm run build 生成到 dist/）。
package web

import "embed"

//go:embed all:dist
var FS embed.FS
