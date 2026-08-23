// Live updates, the command palette and the findings checklist. Owner: W2-L.
//
// Every page here is server-rendered and stays usable with this file absent or
// with JavaScript off: nothing below is needed to read a gate or record a
// decision. What it adds is the things a static page cannot do — say how old
// what you are looking at is, jump to a job without going through the list, and
// keep count of which findings you have actually read.
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

  // Buttons in the form, plus the buttons elsewhere on the page that submit it
  // by id. The review page's decision bar is the second kind: it is the copy
  // of Approve that stays on screen while the document scrolls, and leaving it
  // enabled while the one in the panel was disabled would mean the whole
  // staleness guard could be walked straight past.
  function buttonsFor(form) {
    var own = form.querySelectorAll("button, input[type=submit]");
    var out = [];
    var i;
    for (i = 0; i < own.length; i++) out.push(own[i]);
    if (form.id) {
      var linked = document.querySelectorAll('[form="' + form.id + '"]');
      for (i = 0; i < linked.length; i++) out.push(linked[i]);
    }
    return out;
  }

  function setFormReason(form, reason) {
    var note = form.querySelector(".live-note");
    var buttons = buttonsFor(form);
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
    var rows = document.querySelectorAll("[data-job]");
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
    // A job the list has never drawn is structural too — but only when the
    // list is unfiltered. With ?show= set, a job missing from the page is the
    // filter working, and reloading for it would loop forever.
    var list = document.querySelector("[data-joblist]");
    if (list && !list.getAttribute("data-show")) {
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

  // The gate cell holds a link, so it is rebuilt with createElement rather
  // than assigned as markup. The gate vocabulary is fixed today, but this is
  // the one cell whose value has structure, so it is the one an
  // assigned-markup shortcut would be reached for first.
  function setGate(row, gate) {
    var cell = row.querySelector("[data-field=gate]");
    if (!cell) return;
    if (!gate) {
      if (cell.textContent.trim()) cell.replaceChildren();
      return;
    }
    var pill = cell.querySelector(".gatepill");
    if (!pill) {
      pill = document.createElement("a");
      pill.className = "gatepill";
      pill.href = "/jobs/" + encodeURIComponent(row.getAttribute("data-job")) + "/gate";
      cell.replaceChildren(pill);
    }
    if (pill.textContent !== gate) pill.textContent = gate;
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

  // --- the findings checklist ---------------------------------------------

  // The boxes in the decision panel are a note to yourself: nothing is posted
  // and no decision is blocked by them. What they buy is the count in the bar
  // under the document, which is the difference between having read four
  // findings and having scrolled past them.
  //
  // The count is computed from the DOM on every change rather than from a
  // counter kept alongside it. A tally that drifts from what is on screen is
  // worse than no tally, and this way there is nothing to drift.
  function setupTriage() {
    var list = document.getElementById("triage");
    var label = document.getElementById("triage-label");
    var bar = document.getElementById("triage-bar");
    var note = document.getElementById("triage-note");
    if (!list || !label || !bar) return;

    var boxes = list.querySelectorAll("input[type=checkbox]");
    if (!boxes.length) return;

    function paint() {
      var done = 0;
      for (var i = 0; i < boxes.length; i++) if (boxes[i].checked) done++;
      label.textContent = done + " of " + boxes.length + " findings marked read";
      bar.style.width = Math.round((done / boxes.length) * 100) + "%";
      if (note) {
        note.textContent = done === boxes.length
          ? "every finding marked read"
          : boxes.length - done + " not marked — the decision is yours either way";
      }
    }

    for (var i = 0; i < boxes.length; i++) boxes[i].addEventListener("change", paint);
    paint();
  }

  // --- the submit page's command preview ----------------------------------

  // Kept in step with the two fields it names, and written with textContent
  // only. The server renders the shape of the command, so a reader with
  // JavaScript off still gets something they can copy and edit.
  function setupSubmitPreview() {
    var out = document.getElementById("submit-cli");
    var target = document.getElementById("target");
    var title = document.getElementById("title");
    if (!out || !target) return;

    function paint() {
      var t = target.value || "<target>";
      var line = "sdlc submit " + t;
      if (title && title.value) line += ' --title "' + title.value + '"';
      out.textContent = line + " --file issue.md";
    }
    target.addEventListener("change", paint);
    if (title) title.addEventListener("input", paint);
    paint();
  }

  // --- the command palette -------------------------------------------------

  // A view over the job list, built in the browser from /api/jobs.json. It is
  // not server-rendered on purpose: a palette baked into every page would be
  // one more thing that can be stale about which jobs exist, and this one is
  // fetched at the moment it is opened.
  var palette = null;

  function openPalette() {
    if (palette) return;
    palette = buildPalette();
    document.body.appendChild(palette.root);
    palette.input.focus();
    loadPaletteJobs();
  }

  function closePalette() {
    if (!palette) return;
    palette.root.remove();
    palette = null;
  }

  function buildPalette() {
    var root = document.createElement("div");
    root.className = "overlay";
    root.addEventListener("mousedown", function (e) {
      if (e.target === root) closePalette();
    });

    var sheet = document.createElement("div");
    sheet.className = "sheet";
    root.appendChild(sheet);

    var q = document.createElement("div");
    q.className = "q";
    var caret = document.createElement("span");
    caret.textContent = ">";
    var input = document.createElement("input");
    input.type = "text";
    input.setAttribute("aria-label", "Search jobs");
    input.placeholder = "job id, title or state";
    q.appendChild(caret);
    q.appendChild(input);
    sheet.appendChild(q);

    var list = document.createElement("div");
    list.className = "list";
    sheet.appendChild(list);

    var p = { root: root, input: input, list: list, items: [], rows: [], at: 0 };
    input.addEventListener("input", function () { paintPalette(p); });
    input.addEventListener("keydown", function (e) { paletteKey(p, e); });
    return p;
  }

  function loadPaletteJobs() {
    var p = palette;
    // The stream has already delivered a list on the job pages, but it is
    // narrowed to one job there. Asking for the whole list is one small
    // request and is the only way the palette can reach a job the current page
    // has never mentioned.
    fetch("/api/jobs.json", { cache: "no-store", credentials: "same-origin" })
      .then(function (res) { return res.ok ? res.json() : null; })
      .then(function (snap) {
        if (!palette || palette !== p || !snap || !snap.jobs) return;
        p.items = snap.jobs;
        paintPalette(p);
      })
      .catch(function () {
        // Nothing to show and nothing to say: the pill already reports whether
        // the server is reachable, and a second error message about it would
        // be the same news twice.
      });
    paintPalette(p);
  }

  function matches(job, needle) {
    if (!needle) return true;
    var hay = (job.id + " " + (job.title || "") + " " + (job.state || "") +
               " " + (job.gate || "") + " " + (job.target || "")).toLowerCase();
    return hay.indexOf(needle) >= 0;
  }

  function paintPalette(p) {
    var needle = p.input.value.trim().toLowerCase();
    var hits = [];
    var i;
    for (i = 0; i < p.items.length && hits.length < 20; i++) {
      if (matches(p.items[i], needle)) hits.push(p.items[i]);
    }
    // Waiting first. The palette is opened most often to get back to a
    // decision, and a job that needs one must not sort below one that does
    // not — the same rule the job list is split by.
    hits.sort(function (a, b) { return (b.gate ? 1 : 0) - (a.gate ? 1 : 0); });

    p.rows = [];
    p.list.replaceChildren();
    if (!hits.length) {
      var empty = document.createElement("div");
      empty.className = "empty";
      empty.textContent = p.items.length ? "No job matches." : "Reading the job list…";
      p.list.appendChild(empty);
      return;
    }
    for (i = 0; i < hits.length; i++) {
      p.list.appendChild(paletteRow(p, hits[i]));
    }
    if (p.at >= p.rows.length) p.at = 0;
    markPaletteRow(p);
  }

  // Built element by element, never from a string: job titles are written by
  // whoever filed the issue.
  function paletteRow(p, job) {
    var a = document.createElement("a");
    a.href = job.gate ? "/jobs/" + encodeURIComponent(job.id) + "/gate"
                      : "/jobs/" + encodeURIComponent(job.id);

    var id = document.createElement("span");
    id.className = "mono";
    id.textContent = job.id;
    a.appendChild(id);

    var title = document.createElement("span");
    title.textContent = job.title || "";
    a.appendChild(title);

    var hint = document.createElement("span");
    hint.className = "hint";
    hint.textContent = job.gate ? job.gate + " gate" : (job.state || "");
    a.appendChild(hint);

    p.rows.push(a);
    return a;
  }

  function markPaletteRow(p) {
    for (var i = 0; i < p.rows.length; i++) {
      if (i === p.at) p.rows[i].classList.add("on");
      else p.rows[i].classList.remove("on");
    }
    if (p.rows[p.at] && p.rows[p.at].scrollIntoView) {
      p.rows[p.at].scrollIntoView({ block: "nearest" });
    }
  }

  function paletteKey(p, e) {
    if (e.key === "ArrowDown") {
      e.preventDefault();
      p.at = Math.min(p.at + 1, p.rows.length - 1);
      markPaletteRow(p);
    } else if (e.key === "ArrowUp") {
      e.preventDefault();
      p.at = Math.max(p.at - 1, 0);
      markPaletteRow(p);
    } else if (e.key === "Enter") {
      e.preventDefault();
      if (p.rows[p.at]) location.href = p.rows[p.at].href;
    } else if (e.key === "Escape") {
      e.preventDefault();
      closePalette();
    }
  }

  // --- keyboard ------------------------------------------------------------

  // Three keys, and every one of them has a control on the page that does the
  // same thing. A shortcut is a shortcut; none of these is the only way to do
  // anything, and none of them decides anything on its own — "a" scrolls the
  // approve button into view and focuses it, it does not approve.
  function setupKeys() {
    var opener = document.getElementById("palette-open");
    if (opener) {
      opener.hidden = false;
      opener.style.display = "inline-flex";
      opener.addEventListener("click", openPalette);
    }

    document.addEventListener("keydown", function (e) {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k") {
        e.preventDefault();
        if (palette) closePalette();
        else openPalette();
        return;
      }
      if (e.key === "Escape") { closePalette(); return; }
      if (e.metaKey || e.ctrlKey || e.altKey) return;
      // Typing "a" into the reject reason must not be a shortcut.
      var t = e.target;
      if (t && (/^(input|textarea|select)$/i.test(t.tagName) || t.isContentEditable)) return;

      var form = null;
      if (e.key === "a") form = document.getElementById("approve-form");
      else if (e.key === "r") form = document.getElementById("reject-form");
      if (!form) return;
      var button = form.querySelector("button[type=submit]");
      if (!button || button.disabled) return;
      e.preventDefault();
      if (button.scrollIntoView) button.scrollIntoView({ block: "center" });
      button.focus();
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
  setupTriage();
  setupSubmitPreview();
  setupKeys();
  connect();
  setInterval(function () {
    // One timer for both: the ages on the page and the pill's own claim go
    // stale by the passage of time alone, with no event to prompt a repaint.
    paintTimes(document);
    paintPill();
  }, PILL_TICK_MS);
})();
