// Live updates. Owner: W2-L.
//
// Every page here is server-rendered and stays usable with this file absent or
// with JavaScript off: nothing below is needed to read a gate or record a
// decision. What it adds is the thing a static page cannot do — say how old
// what you are looking at is.
//
// Two rules run through all of it.
//
// 1. Nothing from the server is ever turned into markup. Job titles, hold
//    reasons and log text are written by issue authors and by agents, and
//    building HTML out of them by string concatenation is an injection with
//    extra steps. Text goes in through textContent and createTextNode, and
//    structure is built with createElement, so there is no path from server
//    data to parsed markup.
//
// 2. A page that has gone stale must not present itself as current. The
//    stream's 15s heartbeat is what makes staleness detectable at all; when it
//    stops, the pill says so and the decision buttons are disabled with the
//    reason beside them. The server refuses a decision whose state has moved
//    regardless — this exists so that refusal is not a surprise, because an
//    operator who clicks Approve on a minute-old page has already decided
//    something about a job that may have moved on without them.
(function () {
  "use strict";

  // Three heartbeats missed is the line between "quiet" and "not connected".
  // One missed ping is normal on a busy machine; three is not.
  var LIVE_MS = 45 * 1000;
  var DOWN_MS = 5 * 60 * 1000;
  var PILL_TICK_MS = 1000;
  var FOLLOW_MS = 2000;
  var RELOAD_THROTTLE_MS = 10 * 1000;

  // lastEventAt starts at load rather than at zero because the page in front of
  // the reader was rendered by the server a moment ago: it is current, whether
  // or not a stream ever connects. If none does, this decays to stale on its
  // own after LIVE_MS, which is the correct claim to end up making.
  var lastEventAt = Date.now();
  var errorSince = null;
  var jobs = Object.create(null); // id -> latest snapshot for that job

  // --- relative time ------------------------------------------------------

  // Relative times are computed here and never on the server: a "4m ago"
  // rendered server-side is wrong the moment it is delivered, and the operator
  // may be reading this through an ssh -L from another timezone. The absolute
  // local time goes in the title so the exact value is still one hover away.
  function humanSince(then, now) {
    var s = Math.round((now - then) / 1000);
    if (s < 0) s = 0;
    if (s < 10) return "just now";
    if (s < 60) return s + "s ago";
    var m = Math.round(s / 60);
    if (m < 60) return m + "m ago";
    var h = Math.floor(m / 60);
    if (h < 24) return h + "h " + (m % 60) + "m ago";
    return Math.floor(h / 24) + "d " + (h % 24) + "h ago";
  }

  function paintTime(el, now) {
    var stamp = el.getAttribute("datetime");
    if (!stamp) return;
    var t = Date.parse(stamp);
    if (isNaN(t)) return;
    el.textContent = humanSince(t, now);
    el.title = new Date(t).toLocaleString();
  }

  function paintTimes(root) {
    var now = Date.now();
    var list = (root || document).querySelectorAll("time[datetime]");
    for (var i = 0; i < list.length; i++) paintTime(list[i], now);
  }

  // --- the pill -----------------------------------------------------------

  // Exactly three states, and each one is a claim the page can keep true.
  function paintPill() {
    var pill = document.getElementById("live-pill");
    var age = Date.now() - lastEventAt;
    var live = errorSince === null && age < LIVE_MS;
    var down = errorSince !== null && Date.now() - errorSince >= DOWN_MS;

    if (pill) {
      var since = humanSince(lastEventAt, Date.now());
      if (live) {
        pill.className = "pill live";
        pill.textContent = "live";
      } else if (down) {
        pill.className = "pill down";
        pill.textContent = "disconnected — last update " + since;
      } else {
        pill.className = "pill stale";
        pill.textContent = "stale — reconnecting — last update " + since;
      }
    }
    // The no-JS refresh link is hidden only while the stream is genuinely
    // live (spec 9.3). It comes back the moment the pill goes amber, because
    // that is exactly when the page has stopped updating itself and clicking
    // something is the only way forward.
    var refresh = document.getElementById("refresh-link");
    if (refresh) refresh.hidden = live;

    paintDecisionForms(live);
  }

  // --- decision forms -----------------------------------------------------

  // A form is disabled for one of two reasons: the page cannot tell whether it
  // is current, or the stream says this job has already moved. Both get the
  // reason rendered next to the button, because a button that is greyed out
  // with no explanation reads as a bug and gets worked around with a reload —
  // which is, in fairness, the right fix, so the text says so.
  function paintDecisionForms(live) {
    var forms = document.querySelectorAll("form[data-gate-state]");
    for (var i = 0; i < forms.length; i++) {
      var form = forms[i];
      var reason = "";
      if (!live) {
        reason = "The live connection is stale, so this page may not show the current state. Reload before deciding.";
      } else {
        var id = jobIdOf(form);
        var seen = id ? jobs[id] : null;
        if (seen && seen.state !== form.getAttribute("data-gate-state")) {
          reason = "This job is now in " + seen.state + ". Reload to decide on the current state.";
        }
      }
      setFormReason(form, reason);
    }
  }

  // The job id comes from the form's action rather than from an attribute of
  // its own: the action already names the job it posts to, so the two cannot
  // disagree about which job is being decided.
  function jobIdOf(form) {
    var m = /^\/jobs\/([^/]+)\//.exec(form.getAttribute("action") || "");
    return m ? decodeURIComponent(m[1]) : null;
  }

  function setFormReason(form, reason) {
    var note = form.querySelector(".live-note");
    var buttons = form.querySelectorAll("button, input[type=submit]");
    var i;
    if (!reason) {
      form.removeAttribute("data-disabled");
      for (i = 0; i < buttons.length; i++) buttons[i].disabled = false;
      if (note) note.remove();
      return;
    }
    form.setAttribute("data-disabled", "");
    for (i = 0; i < buttons.length; i++) buttons[i].disabled = true;
    if (!note) {
      note = document.createElement("p");
      note.className = "muted live-note";
      form.appendChild(note);
    }
    note.textContent = reason;
  }

  // --- snapshots ----------------------------------------------------------

  // The stream carries a full list every time, never a delta, so applying one
  // is the same operation whether it is the first or the thousandth and a
  // missed event costs nothing.
  function applySnapshot(snap) {
    lastEventAt = Date.now();
    errorSince = null;
    if (!snap || !snap.jobs) return;

    var i;
    var present = Object.create(null);
    for (i = 0; i < snap.jobs.length; i++) {
      var j = snap.jobs[i];
      if (!j || !j.id) continue;
      jobs[j.id] = j;
      present[j.id] = true;
    }

    var structural = false;
    var rows = document.querySelectorAll("tr[data-job]");
    var shown = Object.create(null);
    for (i = 0; i < rows.length; i++) {
      var row = rows[i];
      var id = row.getAttribute("data-job");
      shown[id] = true;
      var job = jobs[id];
      if (!job || !present[id]) continue;
      // "Waiting on you" is a separate block the server builds, so a job that
      // gains or loses a gate has changed which block it belongs in — which is
      // a structural change this cannot make in place without reimplementing
      // the page's ordering rules in the browser.
      if (hadGate(row) !== !!job.gate) structural = true;
      patchRow(row, job);
    }
    if (location.pathname === "/jobs") {
      for (var id2 in present) {
        if (!shown[id2]) structural = true;
      }
    }
    if (structural) reloadSoon();
  }

  function hadGate(row) {
    var cell = row.querySelector("[data-field=gate]");
    return !!(cell && cell.textContent.trim());
  }

  function patchRow(row, job) {
    setField(row, "state", job.state);
    setField(row, "title", job.title);
    setGate(row, job.gate);
    var cell = row.querySelector("[data-field=since]");
    if (cell && job.since) {
      var t = cell.querySelector("time[datetime]");
      if (!t) {
        t = document.createElement("time");
        cell.replaceChildren(t);
      }
      t.setAttribute("datetime", job.since);
      paintTime(t, Date.now());
    }
  }

  function setField(row, name, value) {
    var cell = row.querySelector("[data-field=" + name + "]");
    if (cell && typeof value === "string" && cell.textContent !== value) {
      cell.textContent = value;
    }
  }

  // The gate cell holds a badge element, so it is rebuilt with createElement
  // rather than assigned as markup. The gate vocabulary is fixed today, but
  // this is the one cell whose value has structure, so it is the one an
  // assigned-markup shortcut would be reached for first.
  function setGate(row, gate) {
    var cell = row.querySelector("[data-field=gate]");
    if (!cell) return;
    if (!gate) {
      if (cell.textContent.trim()) cell.replaceChildren();
      return;
    }
    var badge = cell.querySelector(".badge");
    if (!badge) {
      badge = document.createElement("span");
      badge.className = "badge gate";
      cell.replaceChildren(badge);
    }
    if (badge.textContent !== gate) badge.textContent = gate;
  }

  // reloadSoon repaints the page from the server, which is the only thing that
  // knows how the list is grouped and ordered. Throttled through sessionStorage
  // because a reload resets everything this file remembers, and only while the
  // tab is visible: reloading a background tab loses whatever the reader had
  // scrolled to or typed into a form.
  function reloadSoon() {
    if (document.visibilityState === "hidden") return;
    var now = Date.now();
    try {
      var last = parseInt(sessionStorage.getItem("sdlc-live-reload") || "0", 10);
      if (now - last < RELOAD_THROTTLE_MS) return;
      sessionStorage.setItem("sdlc-live-reload", String(now));
    } catch (e) {
      // Private-mode storage throws. A reload without the throttle is still
      // better than a list that never picks up a new job.
    }
    location.reload();
  }

  // --- the stream ---------------------------------------------------------

  // A job page asks for its own job so a busy queue does not push a snapshot of
  // forty other jobs at it every second. The id comes from the path because
  // every one of those pages is /jobs/<id>/something.
  function streamURL() {
    var m = /^\/jobs\/([^/]+)(?:\/|$)/.exec(location.pathname);
    if (m && m[1] !== "") return "/events/stream?job=" + encodeURIComponent(decodeURIComponent(m[1]));
    return "/events/stream";
  }

  function connect() {
    if (typeof EventSource === "undefined") return;
    var es = new EventSource(streamURL());
    es.addEventListener("jobs", function (ev) {
      try {
        applySnapshot(JSON.parse(ev.data));
      } catch (e) {
        // A snapshot this file cannot parse is not a reason to stop watching
        // the connection; the next one is a whole list again.
      }
      paintPill();
    });
    es.addEventListener("ping", function () {
      // The heartbeat carries no state. Its whole job is to be evidence that
      // the connection is alive.
      lastEventAt = Date.now();
      errorSince = null;
      paintPill();
    });
    es.addEventListener("error", function () {
      // EventSource reconnects on its own; what it does not do is tell the
      // reader. This is where the amber state comes from, and errorSince is
      // what eventually turns it red.
      if (errorSince === null) errorSince = Date.now();
      paintPill();
    });
    es.addEventListener("open", function () {
      errorSince = null;
      paintPill();
    });
  }

  // --- log follow ---------------------------------------------------------

  // The one live-output exception (spec §9.4): a byte offset and a poll, no
  // server-side tailing and no goroutine per reader.
  function setupFollow() {
    var pre = document.getElementById("logtail");
    var box = document.getElementById("follow");
    if (!pre || !box || !pre.getAttribute("data-url")) return;

    var timer = null;
    var busy = false;

    function atBottom() {
      return window.innerHeight + window.scrollY >= document.body.scrollHeight - 40;
    }

    function poll() {
      if (busy || !box.checked) return;
      busy = true;
      var url = pre.getAttribute("data-url") + "?from=" + encodeURIComponent(pre.getAttribute("data-offset") || "0");
      fetch(url, { cache: "no-store", credentials: "same-origin" })
        .then(function (res) {
          if (!res.ok) throw new Error(String(res.status));
          return res.json();
        })
        .then(function (chunk) {
          var stick = atBottom();
          if (chunk.restarted) {
            // The file was rotated or rewritten under us, so these bytes are
            // the file from 0 rather than a continuation. Appending them would
            // show the beginning twice and look like a duplicated run.
            pre.replaceChildren();
          }
          if (chunk.text) pre.appendChild(document.createTextNode(chunk.text));
          pre.setAttribute("data-offset", String(chunk.offset));
          if (stick) window.scrollTo(0, document.body.scrollHeight);
          busy = false;
          // More was waiting than one response carries: ask again straight
          // away rather than falling a poll interval further behind.
          if (chunk.truncated && box.checked) poll();
        })
        .catch(function () {
          // A failed poll is not fatal — the offset is unchanged, so the next
          // one asks for the same bytes. The pill already reports whether the
          // server is reachable at all.
          busy = false;
        });
    }

    box.addEventListener("change", function () {
      if (box.checked) {
        timer = setInterval(poll, FOLLOW_MS);
        poll();
      } else if (timer !== null) {
        clearInterval(timer);
        timer = null;
      }
    });
  }

  // --- start --------------------------------------------------------------

  paintTimes(document);
  paintPill();
  setupFollow();
  connect();
  setInterval(function () {
    // One timer for both: the ages on the page and the pill's own claim go
    // stale by the passage of time alone, with no event to prompt a repaint.
    paintTimes(document);
    paintPill();
  }, PILL_TICK_MS);
})();
