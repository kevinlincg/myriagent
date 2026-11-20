// Package myriagent 只放共用的嵌入資源：介面網頁（web/）。程式本體在 cmd/myriagent 與 internal/ 底下。
package myriagent

import "embed"

// Web 是介面網頁（桌面 app 與 Hub 的網頁版共用同一份；網頁 ↔ 宿主的訊息協定見 docs/web-protocol.md）
//
//go:embed web
var Web embed.FS
