// 伏せ字の入力欄の「表示」ボタン
document.querySelectorAll("button.reveal").forEach(function (btn) {
  btn.addEventListener("click", function () {
    var input = document.getElementById(btn.dataset.target);
    var show = input.type === "password";
    input.type = show ? "text" : "password";
    btn.textContent = show ? "隠す" : "表示";
  });
});

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

// お気に入り・後で読む：ページを再読み込みせずに切り替える（保存一覧では外した記事を消す）
document.querySelectorAll("form.savebtn").forEach(function (form) {
  form.addEventListener("submit", function (ev) {
    ev.preventDefault();
    var btn = form.querySelector("button");
    btn.disabled = true;
    fetch(form.action, {
      method: "POST",
      body: new URLSearchParams(new FormData(form)),
      headers: { Accept: "application/json" },
      credentials: "same-origin",
    })
      .then(function (r) {
        if (!r.ok) throw new Error(r.status);
        return r.json();
      })
      .then(function (res) {
        btn.classList.toggle("on", res.on);
        btn.setAttribute("aria-pressed", String(res.on));
        var card = form.closest("[data-saved-kind]");
        var kind = form.querySelector("input[name=kind]").value;
        if (card && !res.on && card.dataset.savedKind === kind) card.remove();
      })
      .catch(function () { form.submit(); })
      .finally(function () { btn.disabled = false; });
  });
});
