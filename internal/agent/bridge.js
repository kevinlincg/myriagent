// 讓同一份介面網頁在一般瀏覽器（與 Mac 的薄視窗）裡跑：網頁原本對 WebView2 的 window.chrome.webview 收送訊息，
// 這裡以 WebSocket（/ws）接到 myriagent agent。agent 送來的訊息照 WebView2 的樣子包成 { data } 交給訂閱者。
// 連線斷了每 2 秒重試；重新連上就重新載入網頁（狀態都在 agent，網頁重新要一次 init 最單純）。
(function () {
  'use strict';
  var listeners = [];
  var queue = [];
  var ws = null;
  var everOpen = false;

  function url() {
    return (location.protocol === 'https:' ? 'wss://' : 'ws://') + location.host + '/ws';
  }

  function deliver(data) {
    for (var i = 0; i < listeners.length; i++) {
      try { listeners[i]({ data: data }); } catch (e) { console.error(e); }
    }
  }

  function connect() {
    var sock = new WebSocket(url());
    ws = sock;
    sock.onopen = function () {
      if (everOpen) { location.reload(); return; }
      everOpen = true;
      var pending = queue.splice(0);
      for (var i = 0; i < pending.length; i++) sock.send(JSON.stringify(pending[i]));
    };
    sock.onmessage = function (e) {
      var data;
      try { data = JSON.parse(e.data); } catch (err) { return; }
      deliver(data);
    };
    sock.onclose = function () {
      ws = null;
      setTimeout(connect, 2000);
    };
  }

  window.chrome = window.chrome || {};
  window.chrome.webview = {
    postMessage: function (msg) {
      if (ws && ws.readyState === 1) ws.send(JSON.stringify(msg));
      else queue.push(msg);
    },
    addEventListener: function (type, fn) { if (type === 'message' && typeof fn === 'function') listeners.push(fn); },
    removeEventListener: function (type, fn) {
      if (type !== 'message') return;
      var i = listeners.indexOf(fn);
      if (i >= 0) listeners.splice(i, 1);
    },
  };
  connect();
})();
