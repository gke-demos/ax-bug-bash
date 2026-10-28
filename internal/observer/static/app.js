(function () {
  "use strict";

  var SVG_NS = "http://www.w3.org/2000/svg";
  var currentState = null;
  var selectedChildService = "";
  var selectedNodeType = "child"; // "root" or "child"
  var selectedHarnessMode = "multi";

  function normalizeRoute(pathname) {
    if (pathname === "/workers" || pathname.indexOf("/workers/") === 0) {
      return "/workers";
    }
    if (pathname === "/about" || pathname.indexOf("/about/") === 0) {
      return "/about";
    }
    return "/";
  }

  function applyRoute(pathname, pushHistory) {
    var route = normalizeRoute(pathname);
    if (pushHistory && window.location.pathname !== route) {
      window.history.pushState({ route: route }, "", route);
    }

    var navLinks = document.querySelectorAll(".nav-link[data-route]");
    navLinks.forEach(function (link) {
      var r = link.getAttribute("data-route");
      if (r === route) {
        link.classList.add("active");
      } else {
        link.classList.remove("active");
      }
    });

    var views = document.querySelectorAll(".route-view[data-view]");
    views.forEach(function (v) {
      if (v.getAttribute("data-view") === route) {
        v.classList.add("active");
      } else {
        v.classList.remove("active");
      }
    });
  }

  function setTheme(theme) {
    var mode = theme === "light" ? "light" : "dark";
    document.documentElement.setAttribute("data-theme", mode);
    var btnDark = document.getElementById("btn-theme-dark");
    var btnLight = document.getElementById("btn-theme-light");
    if (btnDark && btnLight) {
      if (mode === "dark") {
        btnDark.classList.add("active");
        btnLight.classList.remove("active");
      } else {
        btnLight.classList.add("active");
        btnDark.classList.remove("active");
      }
    }
  }

  function openCmdKModal() {
    var modal = document.getElementById("cmd-k-modal");
    if (modal) {
      modal.classList.remove("hidden");
    }
  }

  function closeCmdKModal() {
    var modal = document.getElementById("cmd-k-modal");
    if (modal) {
      modal.classList.add("hidden");
    }
  }

  function createEl(tag, className, text) {
    var el = document.createElement(tag);
    if (className) {
      el.className = className;
    }
    if (typeof text === "string") {
      el.textContent = text;
    }
    return el;
  }

  function createSvgEl(tag, attrs) {
    var el = document.createElementNS(SVG_NS, tag);
    if (attrs) {
      Object.keys(attrs).forEach(function (k) {
        el.setAttribute(k, String(attrs[k]));
      });
    }
    return el;
  }

  function renderState(state) {
    if (!state) {
      return;
    }
    currentState = state;

    // Update External Surface Links
    if (state.external_links) {
      var sfLink = document.getElementById("link-storefront");
      if (sfLink && state.external_links.storefront_url) {
        sfLink.setAttribute("href", state.external_links.storefront_url);
      }
      var gfLink = document.getElementById("link-grafana");
      if (gfLink && state.external_links.grafana_url) {
        gfLink.setAttribute("href", state.external_links.grafana_url);
      }
    }

    // Update Model Gateway Badges
    if (state.gateway) {
      var provBadge = document.getElementById("badge-provenance");
      if (provBadge) {
        provBadge.textContent = state.gateway.source_provenance || "FIXTURE";
      }
      var modelBadge = document.getElementById("badge-active-model");
      if (modelBadge) {
        modelBadge.textContent = state.gateway.active_model || "gemini-3.8-flash";
      }
    }

    renderLeftColumn(state);
    renderCenterTree(state);
    renderRightInspector(state);
    renderWorkersView(state);
  }

  function renderLeftColumn(state) {
    var alertContent = document.getElementById("alert-webhook-content");
    var tracePre = document.getElementById("loki-trace-pre");
    var blastGrid = document.getElementById("service-blast-grid");

    if (alertContent) {
      alertContent.replaceChildren();
      var latestAlert = state.latest_alert;
      if (!latestAlert) {
        alertContent.textContent = "No firing Alertmanager alerts yet. Press ⌘K to trigger an Incident Scenario.";
      } else {
        var row1 = createEl("div", "service-card");
        var leftMeta = createEl("div", "service-meta");
        leftMeta.appendChild(createEl("span", "service-name", latestAlert.alert_name + " (" + latestAlert.incident_id + ")"));
        leftMeta.appendChild(createEl("span", "service-lang", latestAlert.summary || ""));
        var statusPill = createEl(
          "span",
          latestAlert.status === "resolved" ? "pill pill-healed" : "pill pill-alert",
          (latestAlert.status || "firing").toUpperCase()
        );
        row1.appendChild(leftMeta);
        row1.appendChild(statusPill);
        alertContent.appendChild(row1);
      }
    }

    if (tracePre) {
      var latest = state.latest_alert;
      if (latest && latest.stack_traces && latest.stack_traces.length > 0) {
        tracePre.textContent = latest.stack_traces.join("\n\n---\n\n");
      } else if (state.latest_tree && state.latest_tree.child_tasks && state.latest_tree.child_tasks.length > 0) {
        var traces = [];
        state.latest_tree.child_tasks.forEach(function (c) {
          if (c.task_config && c.task_config.stack_trace) {
            traces.push(c.task_config.stack_trace);
          }
        });
        tracePre.textContent = traces.length > 0 ? traces.join("\n\n") : "Awaiting StorefrontHTTP500Spike telemetry...";
      } else {
        tracePre.textContent = "Awaiting StorefrontHTTP500Spike telemetry...";
      }
    }

    if (blastGrid && Array.isArray(state.services)) {
      blastGrid.replaceChildren();
      state.services.forEach(function (svc) {
        var cls = "service-card";
        if (svc.status === "IMPLICATED") {
          cls += " implicated";
        } else if (svc.status === "HEALED") {
          cls += " healed";
        }
        var card = createEl("div", cls);
        var meta = createEl("div", "service-meta");
        meta.appendChild(createEl("span", "service-name", svc.service));
        meta.appendChild(createEl("span", "service-lang", svc.language + " · 200 OK: " + svc.http_200_count + " · 500 Err: " + svc.http_500_count));
        var badgeCls = "pill";
        if (svc.status === "IMPLICATED") {
          badgeCls = "pill pill-alert";
        } else if (svc.status === "HEALED") {
          badgeCls = "pill pill-healed";
        }
        var badge = createEl("span", badgeCls, svc.status);
        card.appendChild(meta);
        card.appendChild(badge);
        blastGrid.appendChild(card);
      });
    }
  }

  function renderCenterTree(state) {
    var svg = document.getElementById("bugbash-tree-svg");
    var phaseBadge = document.getElementById("tree-phase-badge");
    var titleEl = document.getElementById("tree-incident-title");
    var summaryEl = document.getElementById("tree-incident-summary");
    if (!svg) {
      return;
    }
    svg.replaceChildren();

    var tree = state.latest_tree;
    if (!tree) {
      if (phaseBadge) {
        phaseBadge.textContent = "READY";
      }
      var idleGroup = createSvgEl("g", { transform: "translate(230, 130)" });
      idleGroup.appendChild(createSvgEl("rect", { x: 0, y: 0, width: 300, height: 86, rx: 10, class: "tree-node-rect" }));
      var t1 = createSvgEl("text", { x: 20, y: 34, class: "tree-node-title" });
      t1.textContent = "Root Task (triage-standby)";
      var t2 = createSvgEl("text", { x: 20, y: 58, class: "tree-node-sub" });
      t2.textContent = "Press ⌘K to trigger an Incident Scenario";
      idleGroup.appendChild(t1);
      idleGroup.appendChild(t2);
      svg.appendChild(idleGroup);
      return;
    }

    if (phaseBadge) {
      phaseBadge.textContent = tree.phase || "HEALED";
    }
    if (titleEl) {
      titleEl.textContent = tree.incident_id + " — " + (tree.title || "Recursive Bug Bash Tree");
    }
    if (summaryEl) {
      summaryEl.textContent = tree.summary || "";
    }

    var children = tree.child_tasks || [];
    if (!selectedChildService && children.length > 0) {
      selectedChildService = children[0].service;
    } else if (children.length > 0) {
      var found = children.some(function (c) { return c.service === selectedChildService; });
      if (!found) {
        selectedChildService = children[0].service;
      }
    }

    var rootX = 30;
    var rootY = 128;
    var rootW = 240;
    var rootH = 98;

    // Draw Bezier edges from Root Task to each Child Task
    children.forEach(function (child, idx) {
      var count = children.length;
      var slotH = 320 / Math.max(count, 1);
      var childY = Math.round(slotH * idx + (slotH - 92) / 2) + 14;
      var childX = 410;

      var startX = rootX + rootW;
      var startY = rootY + rootH / 2;
      var endX = childX;
      var endY = childY + 46;
      var ctrlX1 = startX + 70;
      var ctrlX2 = endX - 70;

      var d = "M " + startX + " " + startY + " C " + ctrlX1 + " " + startY + ", " + ctrlX2 + " " + endY + ", " + endX + " " + endY;
      svg.appendChild(createSvgEl("path", { d: d, class: "tree-edge" }));
    });

    // Draw Root Task Node
    var rootGroup = createSvgEl("g", {
      transform: "translate(" + rootX + "," + rootY + ")",
      class: selectedNodeType === "root" ? "tree-node-group selected" : "tree-node-group",
    });
    rootGroup.appendChild(createSvgEl("rect", { x: 0, y: 0, width: rootW, height: rootH, rx: 10, class: "tree-node-rect" }));
    var rtTitle = createSvgEl("text", { x: 14, y: 26, class: "tree-node-title" });
    rtTitle.textContent = tree.root_task.task_name;
    var rtPhase = createSvgEl("text", { x: 14, y: 46, class: "tree-node-sub" });
    rtPhase.textContent = "Phase: " + tree.root_task.phase + " · " + (tree.root_task.worker_ip || "10.244.1.11");
    var rtFanout = createSvgEl("text", { x: 14, y: 66, class: "tree-node-sub" });
    rtFanout.textContent = "Spawned: " + children.length + " Child Tasks (of 5 svcs)";
    var rtCommit = createSvgEl("text", { x: 14, y: 84, class: "tree-node-sub" });
    rtCommit.textContent = "Merged SHA: " + (tree.merged_git_commit_sha || "pending");
    rootGroup.appendChild(rtTitle);
    rootGroup.appendChild(rtPhase);
    rootGroup.appendChild(rtFanout);
    rootGroup.appendChild(rtCommit);
    rootGroup.addEventListener("click", function () {
      selectedNodeType = "root";
      renderState(currentState);
    });
    svg.appendChild(rootGroup);

    // Draw Child Task Nodes
    children.forEach(function (child, idx) {
      var count = children.length;
      var slotH = 320 / Math.max(count, 1);
      var childY = Math.round(slotH * idx + (slotH - 92) / 2) + 14;
      var childX = 410;
      var isSelected = selectedNodeType === "child" && child.service === selectedChildService;

      var cGroup = createSvgEl("g", {
        transform: "translate(" + childX + "," + childY + ")",
        class: isSelected ? "tree-node-group selected" : "tree-node-group",
      });
      cGroup.appendChild(createSvgEl("rect", { x: 0, y: 0, width: 325, height: 92, rx: 10, class: "tree-node-rect" }));

      var cTitle = createSvgEl("text", { x: 14, y: 24, class: "tree-node-title" });
      cTitle.textContent = child.task_name + " (" + child.service + ")";
      var cSub1 = createSvgEl("text", { x: 14, y: 44, class: "tree-node-sub" });
      cSub1.textContent = "Harness: " + child.harness + " · Model: " + (child.active_model || child.requested_model);
      var cSub2 = createSvgEl("text", { x: 14, y: 62, class: "tree-node-sub" });
      cSub2.textContent = "Phase: " + child.phase + " · Worker: " + (child.worker_ip || "10.244.1.12");
      var cSub3 = createSvgEl("text", { x: 14, y: 80, class: "tree-node-sub" });
      cSub3.textContent = "TDD: Red -> Green -> Diff -> Suspend (" + (child.slot_reclamation_ms || 4) + "ms)";

      cGroup.appendChild(cTitle);
      cGroup.appendChild(cSub1);
      cGroup.appendChild(cSub2);
      cGroup.appendChild(cSub3);

      cGroup.addEventListener("click", function () {
        selectedNodeType = "child";
        selectedChildService = child.service;
        renderState(currentState);
      });
      svg.appendChild(cGroup);
    });
  }

  function renderRightInspector(state) {
    var tabsEl = document.getElementById("child-service-tabs");
    var metaEl = document.getElementById("selected-child-meta");
    var regDiffEl = document.getElementById("regression-test-diff");
    var fixDiffEl = document.getElementById("code-fix-diff");
    var testOutEl = document.getElementById("test-runner-output");
    var configPre = document.getElementById("task-config-json");
    var slotBadge = document.getElementById("selected-child-slot-badge");
    var rolloutContent = document.getElementById("rollout-status-content");

    var tree = state.latest_tree;
    if (!tree || !Array.isArray(tree.child_tasks) || tree.child_tasks.length === 0) {
      if (tabsEl) tabsEl.replaceChildren();
      return;
    }

    var children = tree.child_tasks;
    var activeChild = children[0];
    children.forEach(function (c) {
      if (c.service === selectedChildService) {
        activeChild = c;
      }
    });

    if (tabsEl) {
      tabsEl.replaceChildren();
      children.forEach(function (c) {
        var btn = createEl(
          "button",
          c.service === activeChild.service ? "service-tab active" : "service-tab",
          c.service + " (" + c.harness + ")"
        );
        btn.setAttribute("type", "button");
        btn.addEventListener("click", function () {
          selectedNodeType = "child";
          selectedChildService = c.service;
          renderState(currentState);
        });
        tabsEl.appendChild(btn);
      });
    }

    // Update TDD Stepper for activeChild
    var completedPhases = {};
    if (Array.isArray(activeChild.tdd_steps)) {
      activeChild.tdd_steps.forEach(function (st) {
        completedPhases[st.phase] = true;
      });
    }
    var stepEls = document.querySelectorAll("#tdd-stepper .tdd-step");
    stepEls.forEach(function (el) {
      var p = el.getAttribute("data-step");
      if (completedPhases[p]) {
        el.classList.add("done");
      } else {
        el.classList.remove("done");
      }
    });

    if (slotBadge) {
      slotBadge.textContent = "Slot Reclaimed: " + (activeChild.slot_reclamation_ms || 4) + "ms (" + (activeChild.worker_id || "ateworker-gvisor") + ")";
    }

    if (metaEl) {
      metaEl.textContent =
        "Task: " + activeChild.task_name +
        " · Service: " + activeChild.service + " (" + activeChild.language + ")" +
        " · Harness: " + activeChild.harness +
        " · Test: " + activeChild.regression_test_name;
    }

    if (configPre) {
      var cfgObj = selectedNodeType === "root" ? tree.root_task.task_config : activeChild.task_config;
      configPre.textContent = JSON.stringify(cfgObj || {}, null, 2);
    }

    if (regDiffEl) {
      regDiffEl.textContent = activeChild.regression_test_diff || "No regression test diff available.";
    }
    if (fixDiffEl) {
      fixDiffEl.textContent = activeChild.code_fix_diff || activeChild.unified_git_diff || "No code fix diff available.";
    }
    if (testOutEl) {
      var combinedTestOut =
        "=== 1. REPRODUCE RED (Expected Failure) ===\n" +
        (activeChild.red_test_output || "") +
        "\n=== 2. PATCH GREEN (All Tests Passing) ===\n" +
        (activeChild.green_test_output || "");
      testOutEl.textContent = combinedTestOut;
    }

    if (rolloutContent) {
      rolloutContent.replaceChildren();
      var ro = state.latest_rollout;
      if (ro) {
        var summaryLine = createEl(
          "div",
          "service-name",
          "Commit " + ro.commit_sha + " rolled out in " + ro.total_duration_ms + "ms (< 10s SLA: " + (ro.under_ten_second_sla ? "PASS" : "FAIL") + ") · Storefront HTTP 200 Verified: " + (ro.storefront_checkout_verified ? "YES" : "NO")
        );
        rolloutContent.appendChild(summaryLine);
      } else {
        rolloutContent.textContent = "Awaiting Root Task patch merge and push to in-cluster Storefront Git server.";
      }
    }
  }

  function renderWorkersView(state) {
    var pool = state.worker_pool;
    if (!pool) {
      return;
    }
    var kpiTasks = document.getElementById("kpi-task-counts");
    if (kpiTasks) {
      kpiTasks.textContent = pool.active_tasks_count + " Active / " + pool.suspended_tasks_count + " Suspended";
    }
    var kpiReclaimed = document.getElementById("kpi-reclaimed-slots");
    if (kpiReclaimed) {
      kpiReclaimed.textContent = pool.total_reclaimed_slots + " Slots Reclaimed via SuspendTask";
    }
    var kpiRes = document.getElementById("kpi-resource-usage");
    if (kpiRes) {
      kpiRes.textContent =
        pool.allocated_cpu_millicores + "m / " + pool.total_cpu_millicores + "m CPU · " +
        pool.allocated_memory_mb + "MB / " + pool.total_memory_mb + "MB";
    }

    var grid = document.getElementById("workers-grid");
    var shelf = document.getElementById("snapshot-shelf");
    if (grid && Array.isArray(pool.workers)) {
      grid.replaceChildren();
      pool.workers.forEach(function (w) {
        var card = createEl("div", "worker-card");
        var header = createEl("div", "worker-header");
        header.appendChild(createEl("strong", "service-name", w.worker_id + " (" + w.worker_ip + ")"));
        header.appendChild(createEl("span", "pill pill-substrate", w.status));
        card.appendChild(header);

        card.appendChild(
          createEl(
            "div",
            "service-lang",
            "CPU: " + w.used_cpu_millicores + "m / " + w.total_cpu_millicores + "m · Memory: " + w.used_memory_mb + "MB / " + w.total_memory_mb + "MB"
          )
        );
        var cpuPct = Math.min(100, Math.round((w.used_cpu_millicores / Math.max(w.total_cpu_millicores, 1)) * 100));
        var bar = createEl("div", "meter-bar");
        var fill = createEl("div", "meter-fill");
        fill.style.width = cpuPct + "%";
        bar.appendChild(fill);
        card.appendChild(bar);

        var activeList = (w.active_tasks && w.active_tasks.length > 0) ? w.active_tasks.join(", ") : "None (Warm Idle)";
        var snapList = (w.suspended_snapshots && w.suspended_snapshots.length > 0) ? w.suspended_snapshots.join(", ") : "None";
        card.appendChild(createEl("div", "service-lang", "Active Sandboxes: " + activeList));
        card.appendChild(createEl("div", "service-lang", "Volume Snapshots: " + snapList + " · Reclaimed: " + w.reclaimed_slots_count));
        grid.appendChild(card);
      });
    }

    if (shelf) {
      shelf.replaceChildren();
      var snaps = state.volume_snapshots || [];
      if (snaps.length === 0) {
        shelf.appendChild(createEl("div", "empty-hint", "No suspended /workspace/.ax-state volume snapshots recorded yet."));
      } else {
        snaps.forEach(function (sn) {
          var sCard = createEl("div", "worker-card");
          sCard.appendChild(createEl("strong", "service-name", sn.task_name + " (" + sn.service + ")"));
          sCard.appendChild(createEl("div", "service-lang", "Path: " + sn.snapshot_path));
          sCard.appendChild(createEl("div", "service-lang", "Worker: " + sn.worker_id + " · Fidelity: SNAPSHOT_FIDELITY_VOLUMES"));
          shelf.appendChild(sCard);
        });
      }
    }
  }

  function fetchState() {
    fetch("/api/state")
      .then(function (r) { return r.json(); })
      .then(function (data) { renderState(data); })
      .catch(function () {});
  }

  function triggerIncident(incidentID) {
    closeCmdKModal();
    fetch("/api/incidents/trigger", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        incident_id: incidentID,
        harness: selectedHarnessMode,
      }),
    })
      .then(function (r) { return r.json(); })
      .then(function () { fetchState(); })
      .catch(function () {});
  }

  function resetIncidents() {
    closeCmdKModal();
    fetch("/api/incidents/reset", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({}),
    })
      .then(function (r) { return r.json(); })
      .then(function () { fetchState(); })
      .catch(function () {});
  }

  function initSSE() {
    if (typeof window.EventSource !== "function") {
      return;
    }
    var es = new window.EventSource("/api/events");
    var refreshEvents = ["state.snapshot", "alert.fired", "tree.updated", "child.tdd_step", "worker.updated", "rollout.completed"];
    refreshEvents.forEach(function (evtName) {
      es.addEventListener(evtName, function (ev) {
        if (evtName === "state.snapshot" && ev.data) {
          try {
            var parsed = JSON.parse(ev.data);
            renderState(parsed);
            return;
          } catch (e) {}
        }
        fetchState();
      });
    });
  }

  document.addEventListener("DOMContentLoaded", function () {
    applyRoute(window.location.pathname, false);

    window.addEventListener("popstate", function () {
      applyRoute(window.location.pathname, false);
    });

    document.querySelectorAll(".nav-link[data-route], .brand-link[data-nav]").forEach(function (link) {
      link.addEventListener("click", function (e) {
        e.preventDefault();
        var target = link.getAttribute("data-route") || link.getAttribute("data-nav") || "/";
        applyRoute(target, true);
      });
    });

    var btnDark = document.getElementById("btn-theme-dark");
    var btnLight = document.getElementById("btn-theme-light");
    if (btnDark) btnDark.addEventListener("click", function () { setTheme("dark"); });
    if (btnLight) btnLight.addEventListener("click", function () { setTheme("light"); });

    var btnCmdK = document.getElementById("btn-cmd-k");
    if (btnCmdK) btnCmdK.addEventListener("click", openCmdKModal);

    var btnClose = document.getElementById("btn-close-modal");
    if (btnClose) btnClose.addEventListener("click", closeCmdKModal);

    var btnConfig = document.getElementById("btn-toggle-config");
    if (btnConfig) {
      btnConfig.addEventListener("click", function () {
        var drawer = document.getElementById("task-config-drawer");
        if (drawer) drawer.classList.toggle("hidden");
      });
    }

    document.addEventListener("keydown", function (e) {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k") {
        e.preventDefault();
        openCmdKModal();
      } else if (e.key === "Escape") {
        closeCmdKModal();
      }
    });

    document.querySelectorAll(".harness-option[data-harness]").forEach(function (btn) {
      btn.addEventListener("click", function () {
        selectedHarnessMode = btn.getAttribute("data-harness") || "multi";
        document.querySelectorAll(".harness-option[data-harness]").forEach(function (b) {
          b.classList.toggle("active", b === btn);
        });
      });
    });

    document.querySelectorAll(".scenario-card[data-incident]").forEach(function (btn) {
      btn.addEventListener("click", function () {
        var inc = btn.getAttribute("data-incident");
        if (inc) triggerIncident(inc);
      });
    });

    var btnRandom = document.getElementById("btn-trigger-random");
    if (btnRandom) {
      btnRandom.addEventListener("click", function () {
        triggerIncident("RANDOM");
      });
    }

    var btnReset = document.getElementById("btn-reset-incidents");
    if (btnReset) {
      btnReset.addEventListener("click", resetIncidents);
    }

    fetchState();
    initSSE();
  });
})();
