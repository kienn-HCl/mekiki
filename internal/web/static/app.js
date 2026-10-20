// 取得・要約実行中は状態をポーリングし、終わったら再読み込みする
(function () {
  var busy = document.getElementById("busy");
  if (!busy) return;
  var timer = setInterval(function () {
    fetch("/api/status", { credentials: "same-origin" })
      .then(function (r) { return r.json(); })
      .then(function (s) {
        if (!s.busy) {
          clearInterval(timer);
          var u = new URL(location.href);
          u.searchParams.delete("msg");
          u.searchParams.delete("err");
          location.replace(u.toString());
        }
      })
      .catch(function () {});
  }, 3000);
})();
