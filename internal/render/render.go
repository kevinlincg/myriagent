// Package render 把文字轉成介面要的 HTML：Markdown（代理的回覆、計畫）與純文字（使用者訊息）。
// 原始 HTML 一律不輸出（goldmark 預設就會把它換成註解），所以結果可以直接放進 innerHTML。
package render

import (
	"bytes"
	"html"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	gmhtml "github.com/yuin/goldmark/renderer/html"
)

var md = goldmark.New(
	goldmark.WithExtensions(extension.GFM),
	goldmark.WithRendererOptions(gmhtml.WithHardWraps()),
)

// Markdown 轉 HTML（失敗時退回純文字）
func Markdown(src string) string {
	var buf bytes.Buffer
	if err := md.Convert([]byte(src), &buf); err != nil {
		return Text(src)
	}
	return buf.String()
}

// Text 把純文字轉成 HTML 段落：跳脫、換行變 <br>
func Text(s string) string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return ""
	}
	return "<p>" + strings.ReplaceAll(html.EscapeString(s), "\n", "<br>") + "</p>"
}
