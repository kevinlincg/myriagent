namespace Myriagent.UI;

/// <summary>
/// 網頁和宿主之間的訊息橋（在每個頁面載入前注入）。
/// 介面網頁（Web/transport.js）只認 WebView2 的 <c>window.chrome.webview</c>：postMessage、postMessageWithAdditionalObjects、
/// addEventListener('message')。這裡在 WKWebView 裡補上同樣形狀的物件，底下走 WKWebView 的 messageHandlers，
/// 網頁那邊一行都不用改，也照樣判斷成桌面版（transport.kind 是 'webview'）。
/// 檔案：WebView2 把拖放、貼上的 File 物件連本機路徑一起交給宿主；WKWebView 的網頁拿不到路徑。
/// 宿主在檔案進到網頁之前（拖進視窗、貼上、選檔）先把「名稱、大小、修改時間 → 代號」登記到這裡（__files），
/// postMessageWithAdditionalObjects 以名稱與大小找到代號送回宿主，宿主再換成路徑（路徑不經過網頁，網頁也塞不進自己的路徑）。
/// 找不到的（剪貼簿的截圖、從別的網頁拖來的圖片）和 WebView2 一樣丟 TypeError，網頁改走內容上傳或說明
/// </summary>
internal static class WebBridge
{
    /// <summary>WKUserContentController 的處理常式名稱（window.webkit.messageHandlers.myriagent）</summary>
    public const string HandlerName = "myriagent";

    public const string Script = """
        (() => {
          'use strict';
          if (window.chrome && window.chrome.webview) return;
          const handler = window.webkit && window.webkit.messageHandlers && window.webkit.messageHandlers.myriagent;
          if (!handler) return;
          const listeners = new Set();
          const files = [];
          const MAX_FILES = 2000;
          const findFile = (file) => {
            let sameName = null;
            for (let i = files.length - 1; i >= 0; i--) {
              const entry = files[i];
              if (entry.name !== file.name || entry.size !== file.size) continue;
              if (Math.abs(entry.modified - file.lastModified) < 2000) return entry;
              if (!sameName) sameName = entry;
            }
            return sameName;
          };
          const webview = {
            postMessage(message) {
              handler.postMessage(JSON.stringify({ message }));
            },
            postMessageWithAdditionalObjects(message, objects) {
              const tokens = [];
              for (const object of Array.from(objects || [])) {
                const entry = object instanceof File ? findFile(object) : null;
                if (!entry) throw new TypeError('additional File object is not a file on the disk');
                tokens.push(entry.token);
              }
              handler.postMessage(JSON.stringify({ message, files: tokens }));
            },
            addEventListener(type, listener) {
              if (type === 'message' && typeof listener === 'function') listeners.add(listener);
            },
            removeEventListener(type, listener) {
              if (type === 'message') listeners.delete(listener);
            },
          };
          Object.defineProperty(webview, '__deliver', {
            value(data) {
              for (const listener of Array.from(listeners)) {
                try { listener({ data }); } catch (error) { setTimeout(() => { throw error; }, 0); }
              }
            },
          });
          Object.defineProperty(webview, '__files', {
            value(list) {
              for (const entry of list) files.push(entry);
              if (files.length > MAX_FILES) files.splice(0, files.length - MAX_FILES);
            },
          });
          const chrome = window.chrome || {};
          chrome.webview = webview;
          window.chrome = chrome;
        })();
        """;
}
