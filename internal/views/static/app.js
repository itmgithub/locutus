// Locutus front-end helpers. No inline scripts are used, so the page can
// run under a strict Content-Security-Policy.
(function () {
  "use strict";

  // Ask for confirmation before submitting forms marked with data-confirm.
  // Forms marked with data-busy="text" run a slow action: once submitted, the
  // submit button is disabled and shows a spinner with that text, so it is
  // clear the action has started and it is not submitted twice.
  document.addEventListener("submit", function (e) {
    var form = e.target;
    var msg = form.getAttribute("data-confirm");
    if (msg && !window.confirm(msg)) {
      e.preventDefault();
      return;
    }
    // A submit button can ask for its own confirmation, when the form has
    // several buttons that do different things.
    var submitter = e.submitter;
    var buttonMsg = submitter && submitter.getAttribute("data-confirm");
    if (buttonMsg && !window.confirm(buttonMsg)) {
      e.preventDefault();
      return;
    }
    // A real restore that overwrites files asks for confirmation first.
    var overwriteMsg = submitter && submitter.getAttribute("data-confirm-overwrite");
    if (overwriteMsg) {
      var overwrite = form.querySelector("input[name=overwrite]");
      var dryRun = form.querySelector("input[name=dry_run]");
      if (overwrite && overwrite.checked && !(dryRun && dryRun.checked) && !window.confirm(overwriteMsg)) {
        e.preventDefault();
        return;
      }
    }
    var busy = form.getAttribute("data-busy");
    if (busy === null) {
      return;
    }
    if (form.dataset.submitted) {
      e.preventDefault();
      return;
    }
    form.dataset.submitted = "1";
    form.setAttribute("aria-busy", "true");
    var button = form.querySelector("button[type=submit], button:not([type])");
    if (button) {
      // Disable after the browser has collected the form data.
      setTimeout(function () {
        button.dataset.label = button.textContent;
        button.disabled = true;
        button.classList.add("busy");
        button.textContent = "";
        var spinner = document.createElement("span");
        spinner.className = "spinner";
        spinner.setAttribute("aria-hidden", "true");
        button.appendChild(spinner);
        button.appendChild(document.createTextNode(busy));
      }, 0);
    }
  });

  // A page restored from the back/forward cache must not keep busy buttons.
  window.addEventListener("pageshow", function (e) {
    if (!e.persisted) {
      return;
    }
    document.querySelectorAll("form[data-submitted]").forEach(function (form) {
      delete form.dataset.submitted;
      form.removeAttribute("aria-busy");
      form.querySelectorAll("button.busy").forEach(function (b) {
        b.classList.remove("busy");
        b.disabled = false;
        b.textContent = b.dataset.label || "";
      });
    });
  });

  // Show form parts that only apply to a selected option:
  // data-show-when="id=value" or "id=value1|value2".
  function syncToggles() {
    document.querySelectorAll("[data-show-when]").forEach(function (el) {
      var parts = el.getAttribute("data-show-when").split("=");
      var input = document.getElementById(parts[0]);
      if (input) {
        el.hidden = parts[1].split("|").indexOf(input.value) < 0;
      }
    });
  }

  // Schedule editor: asks the server for the cron expression, a readable
  // description and the next run times of the current input.
  function initScheduleEditor(ed) {
    var url = ed.getAttribute("data-preview-url");
    var kind = ed.querySelector(".sched-kind");
    var cron = ed.querySelector(".cron-input");
    var desc = ed.querySelector(".sched-desc");
    var expr = ed.querySelector(".sched-expr");
    var next = ed.querySelector(".sched-next");
    var error = ed.querySelector(".sched-error");
    var lastExpr = expr.textContent;
    var timer = null;
    var seq = 0;

    function refresh() {
      var params = new URLSearchParams();
      ed.querySelectorAll("input[name], select[name]").forEach(function (el) {
        if ((el.type === "checkbox" || el.type === "radio") && !el.checked) {
          return;
        }
        params.append(el.name, el.value);
      });
      var mine = ++seq;
      fetch(url + "&" + params.toString(), { credentials: "same-origin" })
        .then(function (r) { return r.json(); })
        .then(function (res) {
          if (mine !== seq) {
            return;
          }
          error.hidden = !res.error;
          error.textContent = res.error || "";
          desc.textContent = res.description || "";
          expr.textContent = res.expr || "";
          next.textContent = res.next ? next.getAttribute("data-label") + ": " + res.next.join(", ") : "";
          if (res.expr) {
            lastExpr = res.expr;
          }
        });
    }

    function schedule() {
      clearTimeout(timer);
      timer = setTimeout(refresh, 250);
    }
    ed.addEventListener("input", schedule);
    ed.addEventListener("change", schedule);
    // Switching to "custom" starts from the expression chosen so far.
    kind.addEventListener("change", function () {
      if (kind.value === "custom" && cron && !cron.dataset.touched) {
        cron.value = lastExpr;
      }
    });
    if (cron) {
      cron.addEventListener("input", function () { cron.dataset.touched = "1"; });
    }
  }

  document.addEventListener("change", syncToggles);

  // Archive browser: loads one directory at a time and lets restorers select
  // files and directories. Selecting a directory includes everything below it.
  function initTree(el) {
    var url = el.getAttribute("data-tree-url");
    var selectable = el.getAttribute("data-selectable") === "1";
    var loadingText = el.getAttribute("data-loading");
    var emptyText = el.getAttribute("data-empty");
    var form = el.closest("form");
    var buttons = form ? form.querySelectorAll("[data-needs-selection]") : [];
    var counter = document.getElementById("selection-count");

    function updateCount() {
      if (!form || !counter) {
        return;
      }
      var n = form.querySelectorAll("input[name=path]:checked:not(:disabled)").length;
      counter.textContent = n ? counter.getAttribute("data-template").replace("%d", n) : "";
      buttons.forEach(function (b) { b.disabled = n === 0; });
    }

    function formatSize(n) {
      var units = ["B", "KiB", "MiB", "GiB", "TiB"];
      var i = 0;
      while (n >= 1024 && i < units.length - 1) {
        n /= 1024;
        i++;
      }
      return (i ? n.toFixed(1) : n) + " " + units[i];
    }

    function message(ul, text, cls) {
      var li = document.createElement("li");
      li.className = cls || "muted";
      li.textContent = text;
      ul.appendChild(li);
    }

    function setDescendants(li, checked) {
      li.querySelectorAll(":scope > ul input[name=path]").forEach(function (cb) {
        cb.checked = checked;
        cb.disabled = checked;
      });
    }

    function loadDir(ul, path, parentChecked) {
      ul.textContent = "";
      message(ul, loadingText);
      fetch(url + "?path=" + encodeURIComponent(path), { credentials: "same-origin" })
        .then(function (r) {
          return r.json().then(function (j) { return { ok: r.ok, body: j }; });
        })
        .then(function (res) {
          ul.textContent = "";
          if (!res.ok) {
            message(ul, res.body.error || "error", "error-text");
            return;
          }
          if (!res.body.items || !res.body.items.length) {
            message(ul, emptyText);
            return;
          }
          res.body.items.forEach(function (it) {
            ul.appendChild(makeNode(it, parentChecked));
          });
          updateCount();
        })
        .catch(function (err) {
          ul.textContent = "";
          message(ul, String(err), "error-text");
        });
    }

    function makeNode(it, parentChecked) {
      var li = document.createElement("li");
      var row = document.createElement("div");
      row.className = "tree-row";
      li.appendChild(row);
      var isDir = it.type === "d";

      var toggle = document.createElement("button");
      toggle.type = "button";
      toggle.className = "tree-toggle";
      toggle.textContent = isDir ? "▸" : "";
      toggle.disabled = !isDir;
      toggle.setAttribute("aria-label", it.name);
      row.appendChild(toggle);

      var cb = null;
      if (selectable) {
        cb = document.createElement("input");
        cb.type = "checkbox";
        cb.name = "path";
        cb.value = it.path;
        cb.checked = parentChecked;
        cb.disabled = parentChecked;
        cb.addEventListener("change", function () {
          setDescendants(li, cb.checked);
          updateCount();
        });
        row.appendChild(cb);
      }

      var name = document.createElement("span");
      name.className = "tree-name " + (isDir ? "dir" : it.type === "l" ? "link" : "file");
      name.textContent = it.name + (it.type === "l" && it.linktarget ? " → " + it.linktarget : "");
      if (isDir) {
        name.addEventListener("click", function () { toggle.click(); });
      }
      row.appendChild(name);

      var meta = document.createElement("span");
      meta.className = "tree-meta";
      meta.textContent = (isDir ? "" : formatSize(it.size) + "  ") + (it.mtime ? it.mtime.replace("T", " ").substring(0, 19) : "");
      row.appendChild(meta);

      if (isDir) {
        var child = null;
        toggle.addEventListener("click", function () {
          if (!child) {
            child = document.createElement("ul");
            child.className = "tree-list";
            li.appendChild(child);
            loadDir(child, it.path, cb ? cb.checked : false);
          } else {
            child.hidden = !child.hidden;
          }
          toggle.textContent = child.hidden ? "▸" : "▾";
        });
      }
      return li;
    }

    el.textContent = "";
    var root = document.createElement("ul");
    root.className = "tree-list root";
    el.appendChild(root);
    loadDir(root, "", false);
  }

  // Restore form: the button says whether it starts a dry run.
  function initRestoreButton(button) {
    var dryRun = document.getElementById("restore-dry-run");
    if (!dryRun) {
      return;
    }
    function sync() {
      button.textContent = button.getAttribute(dryRun.checked ? "data-label-dry" : "data-label-real");
    }
    dryRun.addEventListener("change", sync);
    sync();
  }

  // Forms marked with data-autosubmit (the language menu) are sent as soon
  // as a choice is made; their submit button is only needed without script.
  function initAutosubmit(form) {
    form.querySelectorAll("button[type=submit]").forEach(function (b) { b.hidden = true; });
    form.querySelectorAll("select").forEach(function (sel) {
      sel.addEventListener("change", function () { form.submit(); });
    });
  }

  // Split buttons: the arrow opens or closes the accordion with the options
  // of its action (aria-controls); at most one is open at a time.
  function initSplitToggles() {
    var toggles = document.querySelectorAll(".split-toggle[aria-controls]");
    function setOpen(toggle, open) {
      toggle.setAttribute("aria-expanded", open ? "true" : "false");
      var panel = document.getElementById(toggle.getAttribute("aria-controls"));
      if (panel) {
        panel.classList.toggle("open", open);
      }
    }
    toggles.forEach(function (toggle) {
      toggle.addEventListener("click", function () {
        var open = toggle.getAttribute("aria-expanded") !== "true";
        toggles.forEach(function (other) { setOpen(other, other === toggle && open); });
        var panel = document.getElementById(toggle.getAttribute("aria-controls"));
        var input = open && panel && panel.querySelector("input:not([type=hidden])");
        if (input) {
          input.focus({ preventScroll: true });
        }
      });
    });
  }

  document.addEventListener("DOMContentLoaded", function () {
    syncToggles();
    initSplitToggles();
    document.querySelectorAll("form[data-autosubmit]").forEach(initAutosubmit);
    var restoreButton = document.getElementById("restore-button");
    if (restoreButton) {
      initRestoreButton(restoreButton);
    }
    document.querySelectorAll(".schedule-editor[data-preview-url]").forEach(initScheduleEditor);
    document.querySelectorAll(".tree[data-tree-url]").forEach(initTree);
    var reload = document.querySelector("[data-reload]");
    if (reload) {
      setTimeout(function () { window.location.reload(); }, parseInt(reload.getAttribute("data-reload"), 10) * 1000);
    }
  });
})();
