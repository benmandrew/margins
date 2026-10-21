// A login link carries the secret in the URL fragment, which the browser
// never sends. Submit it with the login form, and drop it from the address
// bar and history first.
"use strict";

(function () {
  const form = document.getElementById("login");
  const secret = decodeURIComponent(location.hash.slice(1));
  if (!form || !secret) return;
  history.replaceState(null, "", location.pathname + location.search);
  form.elements.secret.value = secret;
  form.submit();
})();
