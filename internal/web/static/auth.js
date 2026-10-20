// Passkey login and enrolment. The page has a #login or #enrol button; the
// enrolment secret is in the URL fragment, which the browser never sends.
"use strict";

(function () {
  const status = document.getElementById("status");
  const say = (msg) => { status.textContent = msg; };

  const b64 = {
    decode(s) {
      s = s.replace(/-/g, "+").replace(/_/g, "/");
      while (s.length % 4) s += "=";
      return Uint8Array.from(atob(s), (c) => c.charCodeAt(0)).buffer;
    },
    encode(buf) {
      const bytes = new Uint8Array(buf);
      let s = "";
      for (const b of bytes) s += String.fromCharCode(b);
      return btoa(s).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
    },
  };

  async function post(url, body) {
    const res = await fetch(url, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      credentials: "same-origin",
      body: JSON.stringify(body),
    });
    let data = {};
    try { data = await res.json(); } catch (e) { /* not JSON */ }
    if (!res.ok) throw new Error(data.error || "The server refused the request (" + res.status + ").");
    return data;
  }

  function creationOptions(opts) {
    const pk = opts.publicKey;
    pk.challenge = b64.decode(pk.challenge);
    pk.user.id = b64.decode(pk.user.id);
    for (const c of pk.excludeCredentials || []) c.id = b64.decode(c.id);
    return { publicKey: pk };
  }

  function requestOptions(opts) {
    const pk = opts.publicKey;
    pk.challenge = b64.decode(pk.challenge);
    for (const c of pk.allowCredentials || []) c.id = b64.decode(c.id);
    return { publicKey: pk, mediation: opts.mediation || "optional" };
  }

  function credentialJSON(cred) {
    const r = cred.response;
    const out = {
      id: cred.id,
      rawId: b64.encode(cred.rawId),
      type: cred.type,
      clientExtensionResults: cred.getClientExtensionResults(),
      authenticatorAttachment: cred.authenticatorAttachment || undefined,
      response: { clientDataJSON: b64.encode(r.clientDataJSON) },
    };
    if (r.attestationObject) {
      out.response.attestationObject = b64.encode(r.attestationObject);
      if (r.getTransports) out.response.transports = r.getTransports();
    } else {
      out.response.authenticatorData = b64.encode(r.authenticatorData);
      out.response.signature = b64.encode(r.signature);
      if (r.userHandle) out.response.userHandle = b64.encode(r.userHandle);
    }
    return out;
  }

  function supported() {
    if (window.PublicKeyCredential) return true;
    say("This browser does not support passkeys. Try a current version of Chrome, Edge, Firefox or Safari.");
    return false;
  }

  const login = document.getElementById("login");
  if (login) {
    login.addEventListener("click", async () => {
      if (!supported()) return;
      login.disabled = true;
      try {
        say("Waiting for your passkey…");
        const opts = await post("/login/begin", {});
        const cred = await navigator.credentials.get(requestOptions(opts));
        const next = new URLSearchParams(location.search).get("next") || "/";
        const done = await post("/login/finish?next=" + encodeURIComponent(next), credentialJSON(cred));
        location.assign(done.redirect || "/");
      } catch (e) {
        say(e.name === "NotAllowedError" ? "The login was cancelled or timed out. Try again." : e.message);
        login.disabled = false;
      }
    });
  }

  const enrol = document.getElementById("enrol");
  if (enrol) {
    const secret = decodeURIComponent(location.hash.slice(1));
    if (!secret) {
      say("This link is missing its secret. Use the full link you were sent.");
      enrol.disabled = true;
    }
    const base = "/enrol/" + encodeURIComponent(enrol.dataset.user);
    enrol.addEventListener("click", async () => {
      if (!supported()) return;
      enrol.disabled = true;
      try {
        say("Waiting for your device…");
        const opts = await post(base + "/begin", { secret });
        const cred = await navigator.credentials.create(creationOptions(opts));
        const done = await post(base + "/finish", credentialJSON(cred));
        // Drop the secret from the address bar and history.
        history.replaceState(null, "", base);
        location.assign(done.redirect || "/");
      } catch (e) {
        say(e.name === "NotAllowedError" ? "The passkey was not created. Try again." :
            e.name === "InvalidStateError" ? "This device already has a passkey for this site. Log in instead." : e.message);
        enrol.disabled = false;
      }
    });
  }
})();
