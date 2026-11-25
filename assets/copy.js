// Adds a "Copy" button to each <div class="code"> block. Without JavaScript
// the commands stay selectable as plain text. No network calls.
(function () {
  if (!navigator.clipboard) return;
  document.querySelectorAll('.code').forEach(function (box) {
    var pre = box.querySelector('pre');
    if (!pre) return;
    var btn = document.createElement('button');
    btn.type = 'button';
    btn.className = 'copy';
    btn.textContent = 'Copy';
    btn.setAttribute('aria-label', 'Copy ' + (box.getAttribute('data-label') || 'command'));
    btn.addEventListener('click', function () {
      var text = Array.prototype.map.call(pre.querySelectorAll('code'), function (c) {
        return c.innerText;
      }).join('\n') || pre.innerText;
      navigator.clipboard.writeText(text.trim()).then(function () {
        btn.textContent = 'Copied';
        setTimeout(function () { btn.textContent = 'Copy'; }, 1500);
      }, function () {
        btn.textContent = 'Select and copy';
      });
    });
    box.appendChild(btn);
  });
})();
