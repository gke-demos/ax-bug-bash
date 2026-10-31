(function () {
  "use strict";

  var SVG_NS = "http://www.w3.org/2000/svg";
  var currentState = null;
  var aboutCatalog = null;
  var selectedChildService = "";
  var selectedNodeType = "child"; // "root" or "child"
  var selectedHarnessMode = "multi";

  // /about interactive state
  var activeAboutTab = "topology"; // "topology" | "lifecycle" | "security"
  var selectedAboutScenarioID = ""; // "" (All Healthy) or "INC-101".."INC-104"
  var selectedTopologyNodeID = "ax-observer";
  var activeLifecycleStepIdx = 0;
  var lifecyclePlayTimer = null;
  var selectedCascadeTierIdx = 0;
  var selectedADRCategory = "ALL";

  function normalizeRoute(pathname) {
    if (pathname === "/workers" || pathname === "/workers/") {
      return "/workers";
    }
    if (pathname === "/about" || pathname === "/about/") {
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

    if (route === "/about" && !aboutCatalog) {
      fetchAboutCatalog();
    }
  }

  function setAboutTab(tabName) {
    activeAboutTab = tabName;
    document.querySelectorAll(".about-tab-btn[data-about-tab]").forEach(function (btn) {
      var isMatch = btn.getAttribute("data-about-tab") === tabName;
      btn.classList.toggle("active", isMatch);
      btn.setAttribute("aria-selected", isMatch ? "true" : "false");
    });
    document.querySelectorAll(".about-tab-panel[data-about-panel]").forEach(function (panel) {
      var isMatch = panel.getAttribute("data-about-panel") === tabName;
      panel.classList.toggle("active", isMatch);
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

  function selectHarnessInModal(harness) {
    var norm = harness || "multi";
    selectedHarnessMode = norm;
    document.querySelectorAll(".harness-option[data-harness]").forEach(function (b) {
      b.classList.toggle("active", b.getAttribute("data-harness") === norm);
    });
  }

  function highlightScenarioInModal(incidentID) {
    document.querySelectorAll(".scenario-card[data-incident]").forEach(function (card) {
      card.classList.toggle("active", card.getAttribute("data-incident") === incidentID);
    });
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

    var execBadge = document.getElementById("badge-execution-mode");
    if (execBadge && state.execution_mode) {
      execBadge.textContent = state.execution_mode;
    }

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
    updateAboutStatusStripFromState(state);
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

  /* =========================================================================
   * ROUTE 3: /about — 3-Tab Interactive SVG Architecture Experience
   * ========================================================================= */

  function updateAboutStatusStripFromState(state) {
    if (!state) return;
    var modeEl = document.getElementById("about-stat-mode");
    var slotsEl = document.getElementById("about-stat-slots");
    var harnessEl = document.getElementById("about-stat-harness");
    var modelEl = document.getElementById("about-stat-model");
    if (modeEl && state.execution_mode) {
      modeEl.textContent = state.execution_mode;
    }
    if (slotsEl && state.worker_pool) {
      var workers = state.worker_pool.workers || [];
      var activeCount = state.worker_pool.active_tasks_count || 0;
      slotsEl.textContent = workers.length + " Warm Slots / " + activeCount + " Active";
    }
    if (harnessEl && state.default_harness) {
      harnessEl.textContent = state.default_harness;
    }
    if (modelEl && state.gateway) {
      modelEl.textContent = (state.gateway.active_model || "gemini-3.8-flash") + " (" + (state.gateway.source_provenance || "FIXTURE") + ")";
    }
  }

  function renderAboutExperience(catalog) {
    if (!catalog) return;
    aboutCatalog = catalog;

    if (catalog.status_strip) {
      var st = catalog.status_strip;
      var modeEl = document.getElementById("about-stat-mode");
      var slotsEl = document.getElementById("about-stat-slots");
      var harnessEl = document.getElementById("about-stat-harness");
      var modelEl = document.getElementById("about-stat-model");
      var baseEl = document.getElementById("about-stat-baseline");
      if (modeEl) modeEl.textContent = st.active_execution_mode || "SIMULATED";
      if (slotsEl) slotsEl.textContent = st.warm_idle_slots + " Warm / " + st.active_sandbox_slots + " Active (" + st.reclaimed_warm_slots + " Reclaimed)";
      if (harnessEl) harnessEl.textContent = st.default_harness || "multi";
      if (modelEl) modelEl.textContent = (st.active_cascade_model || "gemini-3.8-flash") + " (" + (st.source_provenance || "FIXTURE") + ")";
      if (baseEl) baseEl.textContent = (st.storefront_baseline_tag || "baseline-healthy") + " (< 10s Selective Reload)";
    }

    renderAboutTopologyTab(catalog);
    renderAboutLifecycleTab(catalog);
    renderAboutSecurityAndADRTab(catalog);
  }

  function renderAboutTopologyTab(catalog) {
    var pillContainer = document.getElementById("about-scenario-pills");
    if (pillContainer) {
      pillContainer.replaceChildren();
      var baseBtn = createEl(
        "button",
        selectedAboutScenarioID === "" ? "service-tab active" : "service-tab",
        "All Healthy (Baseline)"
      );
      baseBtn.setAttribute("type", "button");
      baseBtn.addEventListener("click", function () {
        selectedAboutScenarioID = "";
        renderAboutTopologyTab(catalog);
      });
      pillContainer.appendChild(baseBtn);

      (catalog.scenarios || []).forEach(function (sc) {
        var btn = createEl(
          "button",
          selectedAboutScenarioID === sc.incident_id ? "service-tab active" : "service-tab",
          sc.incident_id + ": " + sc.implicated_services.join(" + ")
        );
        btn.setAttribute("type", "button");
        btn.addEventListener("click", function () {
          selectedAboutScenarioID = sc.incident_id;
          if (sc.implicated_services && sc.implicated_services.length > 0) {
            selectedTopologyNodeID = sc.implicated_services[0];
          }
          renderAboutTopologyTab(catalog);
        });
        pillContainer.appendChild(btn);
      });
    }

    var activeScenario = null;
    (catalog.scenarios || []).forEach(function (sc) {
      if (sc.incident_id === selectedAboutScenarioID) {
        activeScenario = sc;
      }
    });

    var activeNodesMap = {};
    var implicatedMap = {};
    var activeEdgesMap = {};
    if (activeScenario) {
      (activeScenario.active_node_ids || []).forEach(function (id) { activeNodesMap[id] = true; });
      (activeScenario.implicated_services || []).forEach(function (id) { implicatedMap[id] = true; });
      (activeScenario.active_edge_ids || []).forEach(function (id) { activeEdgesMap[id] = true; });
    }

    var svg = document.getElementById("about-topology-svg");
    if (svg) {
      svg.replaceChildren();
      svg.setAttribute("viewBox", "0 0 " + catalog.view_box.width + " " + catalog.view_box.height);

      // 1. Render 4 Swimlanes
      (catalog.swimlanes || []).forEach(function (lane) {
        var laneGroup = createSvgEl("g", { class: "topo-swimlane" });
        laneGroup.appendChild(createSvgEl("rect", {
          x: 16,
          y: lane.y,
          width: catalog.view_box.width - 32,
          height: lane.height,
          rx: 10,
          class: "topo-swimlane-rect",
        }));
        var laneTitle = createSvgEl("text", { x: 30, y: lane.y + 20, class: "topo-swimlane-title" });
        laneTitle.textContent = lane.title + " — " + lane.subtitle;
        laneGroup.appendChild(laneTitle);
        svg.appendChild(laneGroup);
      });

      // 2. Render 11 Signal Edges + Pulse Circles
      (catalog.edges || []).forEach(function (edge) {
        var pts = edge.path_waypoints || [];
        if (pts.length < 2) return;
        var dParts = ["M " + pts[0].x + " " + pts[0].y];
        for (var i = 1; i < pts.length; i++) {
          dParts.push("L " + pts[i].x + " " + pts[i].y);
        }
        var dStr = dParts.join(" ");
        var isEdgeActive = !activeScenario || activeEdgesMap[edge.id];
        var edgeCls = isEdgeActive ? "topo-edge active" : "topo-edge dimmed";
        var pathEl = createSvgEl("path", { id: edge.id, d: dStr, class: edgeCls });
        svg.appendChild(pathEl);

        if (isEdgeActive) {
          var midIdx = Math.floor(pts.length / 2);
          var pA = pts[midIdx - 1] || pts[0];
          var pB = pts[midIdx] || pts[pts.length - 1];
          var midX = Math.round((pA.x + pB.x) / 2);
          var midY = Math.round((pA.y + pB.y) / 2);
          var pulseDot = createSvgEl("circle", {
            cx: midX,
            cy: midY,
            r: 4,
            class: "topo-pulse-dot",
          });
          svg.appendChild(pulseDot);
        }
      });

      // 3. Render 12 Interactive Topology Nodes
      (catalog.nodes || []).forEach(function (node) {
        var cls = "topo-node-group";
        if (node.id === selectedTopologyNodeID) {
          cls += " selected";
        }
        if (activeScenario) {
          if (implicatedMap[node.id]) {
            cls += " blast-implicated";
          } else if (activeNodesMap[node.id]) {
            cls += " blast-active";
          } else {
            cls += " blast-dimmed";
          }
        }

        var g = createSvgEl("g", {
          transform: "translate(" + node.x + "," + node.y + ")",
          class: cls,
          "data-node-id": node.id,
        });
        g.appendChild(createSvgEl("rect", {
          x: 0,
          y: 0,
          width: node.width,
          height: node.height,
          rx: 9,
          class: "topo-node-rect",
        }));

        var titleText = createSvgEl("text", { x: 12, y: 24, class: "tree-node-title" });
        titleText.textContent = node.label;
        var subText = createSvgEl("text", { x: 12, y: 44, class: "tree-node-sub" });
        subText.textContent = node.sublabel;
        var portText = createSvgEl("text", { x: 12, y: 64, class: "topo-node-port" });
        portText.textContent = node.port + " · " + node.code_path;

        g.appendChild(titleText);
        g.appendChild(subText);
        g.appendChild(portText);

        g.addEventListener("click", function () {
          selectedTopologyNodeID = node.id;
          renderAboutTopologyTab(catalog);
        });
        svg.appendChild(g);
      });
    }

    // Update Node & Scenario Inspector Panel
    var selectedNode = (catalog.nodes || [])[0];
    (catalog.nodes || []).forEach(function (n) {
      if (n.id === selectedTopologyNodeID) {
        selectedNode = n;
      }
    });

    var titleEl = document.getElementById("inspector-node-title");
    var badgesEl = document.getElementById("inspector-node-badges");
    var descEl = document.getElementById("inspector-node-desc");
    var capsEl = document.getElementById("inspector-node-caps");
    var scBox = document.getElementById("inspector-scenario-box");
    var scTitle = document.getElementById("inspector-scenario-title");
    var scWhy = document.getElementById("inspector-scenario-why");
    var launchBtn = document.getElementById("btn-launch-from-about");

    if (selectedNode) {
      if (titleEl) titleEl.textContent = selectedNode.label;
      if (badgesEl) {
        badgesEl.replaceChildren();
        badgesEl.appendChild(createEl("span", "pill pill-accent", selectedNode.language));
        badgesEl.appendChild(createEl("span", "pill pill-substrate", selectedNode.port));
        badgesEl.appendChild(createEl("span", "pill", selectedNode.code_path));
      }
      if (descEl) descEl.textContent = selectedNode.description;
      if (capsEl) {
        capsEl.replaceChildren();
        (selectedNode.key_capabilities || []).forEach(function (cap) {
          capsEl.appendChild(createEl("li", "", cap));
        });
      }
    }

    if (scBox) {
      if (activeScenario) {
        scBox.classList.remove("hidden");
        if (scTitle) {
          scTitle.textContent = activeScenario.incident_id + " — " + activeScenario.title + " (" + activeScenario.error_code + ")";
        }
        if (scWhy) {
          scWhy.textContent =
            "Implicated: " + activeScenario.implicated_services.join(", ") +
            " · Unaffected (Untouched): " + activeScenario.unaffected_services.join(", ") +
            " · Why CI Missed It: " + activeScenario.why_ci_missed_it;
        }
        if (launchBtn) {
          launchBtn.onclick = function () {
            selectHarnessInModal(activeScenario.recommended_harness || "multi");
            highlightScenarioInModal(activeScenario.incident_id);
            applyRoute("/", true);
            openCmdKModal();
          };
        }
      } else {
        scBox.classList.add("hidden");
      }
    }
  }

  function renderAboutLifecycleTab(catalog) {
    var stages = catalog.lifecycle_stages || [];
    if (stages.length === 0) return;
    if (activeLifecycleStepIdx < 0 || activeLifecycleStepIdx >= stages.length) {
      activeLifecycleStepIdx = 0;
    }
    var stage = stages[activeLifecycleStepIdx];

    var pillsEl = document.getElementById("lifecycle-stage-pills");
    if (pillsEl) {
      pillsEl.replaceChildren();
      stages.forEach(function (st, idx) {
        var btn = createEl(
          "button",
          idx === activeLifecycleStepIdx ? "lifecycle-pill active" : "lifecycle-pill",
          st.title
        );
        btn.setAttribute("type", "button");
        btn.addEventListener("click", function () {
          stopLifecycleAutoPlay();
          activeLifecycleStepIdx = idx;
          renderAboutLifecycleTab(catalog);
        });
        pillsEl.appendChild(btn);
      });
    }

    var svg = document.getElementById("about-lifecycle-svg");
    if (svg) {
      svg.replaceChildren();
      // Top row: 6 stage pipeline boxes
      stages.forEach(function (st, idx) {
        var boxX = 16 + idx * 182;
        var boxY = 22;
        var boxW = 170;
        var boxH = 88;
        var isCurrent = idx === activeLifecycleStepIdx;
        var isPast = idx < activeLifecycleStepIdx;
        var cls = "lc-stage-node";
        if (isCurrent) cls += " active";
        else if (isPast) cls += " completed";

        var g = createSvgEl("g", { transform: "translate(" + boxX + "," + boxY + ")", class: cls });
        g.appendChild(createSvgEl("rect", { x: 0, y: 0, width: boxW, height: boxH, rx: 9, class: "topo-node-rect" }));
        var t1 = createSvgEl("text", { x: 12, y: 26, class: "tree-node-title" });
        t1.textContent = "Step " + st.step_number;
        var t2 = createSvgEl("text", { x: 12, y: 48, class: "tree-node-sub" });
        t2.textContent = st.title.replace(/^[0-9]+\.\s*/, "").slice(0, 24);
        var t3 = createSvgEl("text", { x: 12, y: 68, class: "topo-node-port" });
        t3.textContent = "Root: " + st.root_task_phase;
        g.appendChild(t1);
        g.appendChild(t2);
        g.appendChild(t3);
        g.addEventListener("click", function () {
          stopLifecycleAutoPlay();
          activeLifecycleStepIdx = idx;
          renderAboutLifecycleTab(catalog);
        });
        svg.appendChild(g);

        if (idx < stages.length - 1) {
          var arrowX1 = boxX + boxW;
          var arrowX2 = boxX + 182;
          svg.appendChild(createSvgEl("path", {
            d: "M " + arrowX1 + " 66 L " + arrowX2 + " 66",
            class: idx <= activeLifecycleStepIdx ? "topo-edge active" : "topo-edge dimmed",
          }));
        }
      });

      // Bottom row: 4 Warm gVisor Worker Slots at this stage
      var slotTitle = createSvgEl("text", { x: 20, y: 155, class: "topo-swimlane-title" });
      slotTitle.textContent = "Agent Substrate v0.6.0 Warm Sandbox Slot Transitions (Step " + stage.step_number + " — Child Phase: " + stage.child_task_phase + ")";
      svg.appendChild(slotTitle);

      (stage.slot_states || []).forEach(function (sl, sIdx) {
        var sx = 20 + sIdx * 272;
        var sy = 175;
        var sw = 256;
        var sh = 128;
        var slCls = "lc-slot-group " + sl.state.toLowerCase();
        var sg = createSvgEl("g", { transform: "translate(" + sx + "," + sy + ")", class: slCls });
        sg.appendChild(createSvgEl("rect", { x: 0, y: 0, width: sw, height: sh, rx: 10, class: "topo-node-rect" }));
        var stName = createSvgEl("text", { x: 14, y: 28, class: "tree-node-title" });
        stName.textContent = sl.slot_id;
        var stState = createSvgEl("text", { x: 14, y: 52, class: "arch-edge-label" });
        stState.textContent = "State: " + sl.state;
        var stRole = createSvgEl("text", { x: 14, y: 76, class: "tree-node-sub" });
        stRole.textContent = sl.task_role;
        var stHarness = createSvgEl("text", { x: 14, y: 100, class: "topo-node-port" });
        stHarness.textContent = "Harness: " + sl.harness + " · SNAPSHOT_FIDELITY_VOLUMES";
        sg.appendChild(stName);
        sg.appendChild(stState);
        sg.appendChild(stRole);
        sg.appendChild(stHarness);
        svg.appendChild(sg);
      });
    }

    var subEl = document.getElementById("lc-stage-subtitle");
    var titleEl = document.getElementById("lc-stage-title");
    var sumEl = document.getElementById("lc-stage-summary");
    var badgesEl = document.getElementById("lc-slot-badges");
    var protoLabelEl = document.getElementById("lc-protocol-label");
    var protoPreEl = document.getElementById("lc-protocol-payload");

    if (subEl) subEl.textContent = "Stage " + stage.step_number + " of " + stages.length + " · " + stage.subtitle;
    if (titleEl) titleEl.textContent = stage.title;
    if (sumEl) sumEl.textContent = stage.summary;
    if (badgesEl) {
      badgesEl.replaceChildren();
      badgesEl.appendChild(createEl("span", "pill pill-accent", "Root Task: " + stage.root_task_phase));
      badgesEl.appendChild(createEl("span", "pill pill-substrate", "Child Tasks: " + stage.child_task_phase));
    }
    if (protoLabelEl) protoLabelEl.textContent = stage.protocol_label;
    if (protoPreEl) {
      try {
        protoPreEl.textContent = JSON.stringify(JSON.parse(stage.protocol_payload_preview), null, 2);
      } catch (e) {
        protoPreEl.textContent = stage.protocol_payload_preview || "{}";
      }
    }
  }

  function stopLifecycleAutoPlay() {
    if (lifecyclePlayTimer) {
      window.clearInterval(lifecyclePlayTimer);
      lifecyclePlayTimer = null;
    }
    var playBtn = document.getElementById("btn-lc-play");
    if (playBtn) playBtn.textContent = "Auto-Play";
  }

  function toggleLifecycleAutoPlay() {
    if (!aboutCatalog) return;
    if (lifecyclePlayTimer) {
      stopLifecycleAutoPlay();
      return;
    }
    var playBtn = document.getElementById("btn-lc-play");
    if (playBtn) playBtn.textContent = "Pause";
    lifecyclePlayTimer = window.setInterval(function () {
      var stages = (aboutCatalog && aboutCatalog.lifecycle_stages) || [];
      if (stages.length === 0) return;
      activeLifecycleStepIdx = (activeLifecycleStepIdx + 1) % stages.length;
      renderAboutLifecycleTab(aboutCatalog);
    }, 2200);
  }

  function renderAboutSecurityAndADRTab(catalog) {
    var cascadeModels = catalog.cascade_models || [];
    var tierSelector = document.getElementById("cascade-tier-selector");
    if (tierSelector) {
      tierSelector.replaceChildren();
      cascadeModels.forEach(function (m, idx) {
        var btn = createEl(
          "button",
          idx === selectedCascadeTierIdx ? "service-tab active" : "service-tab",
          "Tier " + m.tier + ": " + m.model_id
        );
        btn.setAttribute("type", "button");
        btn.addEventListener("click", function () {
          selectedCascadeTierIdx = idx;
          renderAboutSecurityAndADRTab(catalog);
        });
        tierSelector.appendChild(btn);
      });
    }

    var secSvg = document.getElementById("about-security-svg");
    if (secSvg) {
      secSvg.replaceChildren();
      // Hop 1: Airgapped gVisor Sandbox
      var box1 = createSvgEl("g", { transform: "translate(20, 35)" });
      box1.appendChild(createSvgEl("rect", { x: 0, y: 0, width: 290, height: 220, rx: 10, class: "topo-node-rect" }));
      var b1t = createSvgEl("text", { x: 16, y: 30, class: "tree-node-title" });
      b1t.textContent = "1. Airgapped gVisor Sandbox (ateworker)";
      var b1s1 = createSvgEl("text", { x: 16, y: 56, class: "tree-node-sub" });
      b1s1.textContent = "• Zero GCP SA keys or OAuth tokens in env";
      var b1s2 = createSvgEl("text", { x: 16, y: 80, class: "tree-node-sub" });
      b1s2.textContent = "• Holds scoped TaskSessionToken only";
      var b1s3 = createSvgEl("text", { x: 16, y: 104, class: "tree-node-sub" });
      b1s3.textContent = "• OpenFGA Tuple: task:child#owner@task:root";
      var b1s4 = createSvgEl("text", { x: 16, y: 128, class: "topo-node-port" });
      b1s4.textContent = "Harnesses: antigravity | opencode | pi";
      box1.appendChild(b1t);
      box1.appendChild(b1s1);
      box1.appendChild(b1s2);
      box1.appendChild(b1s3);
      box1.appendChild(b1s4);
      secSvg.appendChild(box1);

      secSvg.appendChild(createSvgEl("path", { d: "M 310 145 L 380 145", class: "topo-edge active" }));
      var hop1Lbl = createSvgEl("text", { x: 316, y: 132, class: "arch-edge-label" });
      hop1Lbl.textContent = "Hop 1: Task Token";
      secSvg.appendChild(hop1Lbl);

      // Hop 2: Zero-Secret Model Gateway
      var box2 = createSvgEl("g", { transform: "translate(380, 35)" });
      box2.appendChild(createSvgEl("rect", { x: 0, y: 0, width: 310, height: 220, rx: 10, class: "topo-node-rect" }));
      var b2t = createSvgEl("text", { x: 16, y: 30, class: "tree-node-title" });
      b2t.textContent = "2. Zero-Secret Model Gateway (:8090)";
      var b2s1 = createSvgEl("text", { x: 16, y: 56, class: "tree-node-sub" });
      b2s1.textContent = "• Validates TaskSessionToken & task_name";
      var b2s2 = createSvgEl("text", { x: 16, y: 80, class: "tree-node-sub" });
      b2s2.textContent = "• Normalizes google-genai & openai-compat";
      var b2s3 = createSvgEl("text", { x: 16, y: 104, class: "tree-node-sub" });
      b2s3.textContent = "• Attaches GKE Workload Identity OAuth2";
      var b2s4 = createSvgEl("text", { x: 16, y: 128, class: "topo-node-port" });
      b2s4.textContent = "Compute Obtainability: 4-Tier Cascade";
      box2.appendChild(b2t);
      box2.appendChild(b2s1);
      box2.appendChild(b2s2);
      box2.appendChild(b2s3);
      box2.appendChild(b2s4);
      secSvg.appendChild(box2);

      secSvg.appendChild(createSvgEl("path", { d: "M 690 145 L 760 145", class: "topo-edge active" }));
      var hop2Lbl = createSvgEl("text", { x: 696, y: 132, class: "arch-edge-label" });
      hop2Lbl.textContent = "Hop 2: Workload ID";
      secSvg.appendChild(hop2Lbl);

      // Hop 3: 4-Tier Gemini Flash Cascade on Agent Platform
      cascadeModels.forEach(function (cm, idx) {
        var cy = 35 + idx * 56;
        var isSelectedTier = idx === selectedCascadeTierIdx;
        var is429Simulated = idx < selectedCascadeTierIdx;
        var cg = createSvgEl("g", {
          transform: "translate(760," + cy + ")",
          class: isSelectedTier ? "lc-stage-node active" : "lc-stage-node",
        });
        cg.appendChild(createSvgEl("rect", { x: 0, y: 0, width: 340, height: 48, rx: 8, class: "topo-node-rect" }));
        var ct1 = createSvgEl("text", { x: 12, y: 20, class: "tree-node-title" });
        ct1.textContent = "Tier " + cm.tier + ": " + cm.model_id;
        var ct2 = createSvgEl("text", { x: 12, y: 38, class: "tree-node-sub" });
        if (is429Simulated) {
          ct2.textContent = "HTTP 429 / 503 Quota Exhausted -> Failover Next Tier";
        } else if (isSelectedTier) {
          ct2.textContent = "ACTIVE SERVING TIER (" + cm.latency_target_ms + "ms target)";
        } else {
          ct2.textContent = "Standby Fallback Tier (" + cm.latency_target_ms + "ms)";
        }
        cg.appendChild(ct1);
        cg.appendChild(ct2);
        cg.addEventListener("click", function () {
          selectedCascadeTierIdx = idx;
          renderAboutSecurityAndADRTab(catalog);
        });
        secSvg.appendChild(cg);
      });
    }

    // Render 3 Coding Harness Cards
    var harnessGrid = document.getElementById("about-harnesses-grid");
    if (harnessGrid) {
      harnessGrid.replaceChildren();
      (catalog.harnesses || []).forEach(function (h) {
        var card = createEl("div", "worker-card");
        var top = createEl("div", "worker-header");
        top.appendChild(createEl("strong", "service-name", h.display_name + " (" + h.id + ")"));
        top.appendChild(createEl("span", "pill pill-accent", h.wire_protocol));
        card.appendChild(top);
        card.appendChild(createEl("div", "service-lang", h.strength_summary));
        card.appendChild(createEl("div", "service-lang", "Binary: " + h.binary_path + " · Auth: " + h.auth_header));
        var cmdPre = createEl("pre", "code-block", h.command_template);
        card.appendChild(cmdPre);
        harnessGrid.appendChild(card);
      });
    }

    // Render Filterable 14-ADR Catalog
    var adrs = catalog.adrs || [];
    var filterBar = document.getElementById("adr-category-filters");
    if (filterBar) {
      filterBar.replaceChildren();
      var categories = ["ALL"];
      adrs.forEach(function (a) {
        if (a.category && categories.indexOf(a.category) === -1) {
          categories.push(a.category);
        }
      });
      categories.forEach(function (cat) {
        var btn = createEl(
          "button",
          selectedADRCategory === cat ? "service-tab active" : "service-tab",
          cat === "ALL" ? "All ADRs (" + adrs.length + ")" : cat
        );
        btn.setAttribute("type", "button");
        btn.addEventListener("click", function () {
          selectedADRCategory = cat;
          renderAboutSecurityAndADRTab(catalog);
        });
        filterBar.appendChild(btn);
      });
    }

    var adrGrid = document.getElementById("about-adr-grid");
    if (adrGrid) {
      adrGrid.replaceChildren();
      adrs.forEach(function (adr) {
        if (selectedADRCategory !== "ALL" && adr.category !== selectedADRCategory) {
          return;
        }
        var card = createEl("article", "adr-card");
        var header = createEl("div", "worker-header");
        header.appendChild(createEl("span", "pill pill-accent", adr.id + " · " + adr.category));
        header.appendChild(createEl("span", "pill pill-healed", adr.status));
        card.appendChild(header);

        card.appendChild(createEl("strong", "service-name", adr.title));
        card.appendChild(createEl("div", "service-lang", "File: docs/adr/" + adr.filename));
        card.appendChild(createEl("p", "inspector-desc", adr.summary));

        var ul = createEl("ul", "inspector-list");
        (adr.key_decisions || []).forEach(function (d) {
          ul.appendChild(createEl("li", "", d));
        });
        card.appendChild(ul);

        var countersBar = createEl("div", "about-pill-bar");
        var rc = adr.runtime_counters || {};
        Object.keys(rc).forEach(function (k) {
          countersBar.appendChild(createEl("span", "pill", k + ": " + rc[k]));
        });
        card.appendChild(countersBar);

        adrGrid.appendChild(card);
      });
    }
  }

  function fetchAboutCatalog() {
    fetch("/api/about/catalog")
      .then(function (r) { return r.json(); })
      .then(function (data) { renderAboutExperience(data); })
      .catch(function () {});
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
    var refreshEvents = ["state.snapshot", "alert.fired", "tree.updated", "child.tdd_step", "worker.updated", "rollout.completed", "observer.event"];
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

    document.querySelectorAll(".about-tab-btn[data-about-tab]").forEach(function (btn) {
      btn.addEventListener("click", function () {
        var tab = btn.getAttribute("data-about-tab") || "topology";
        setAboutTab(tab);
      });
    });

    var btnLcPrev = document.getElementById("btn-lc-prev");
    var btnLcPlay = document.getElementById("btn-lc-play");
    var btnLcNext = document.getElementById("btn-lc-next");
    var btnLcReset = document.getElementById("btn-lc-reset");
    if (btnLcPrev) {
      btnLcPrev.addEventListener("click", function () {
        stopLifecycleAutoPlay();
        if (!aboutCatalog || !aboutCatalog.lifecycle_stages) return;
        var len = aboutCatalog.lifecycle_stages.length;
        activeLifecycleStepIdx = (activeLifecycleStepIdx - 1 + len) % len;
        renderAboutLifecycleTab(aboutCatalog);
      });
    }
    if (btnLcPlay) {
      btnLcPlay.addEventListener("click", toggleLifecycleAutoPlay);
    }
    if (btnLcNext) {
      btnLcNext.addEventListener("click", function () {
        stopLifecycleAutoPlay();
        if (!aboutCatalog || !aboutCatalog.lifecycle_stages) return;
        var len = aboutCatalog.lifecycle_stages.length;
        activeLifecycleStepIdx = (activeLifecycleStepIdx + 1) % len;
        renderAboutLifecycleTab(aboutCatalog);
      });
    }
    if (btnLcReset) {
      btnLcReset.addEventListener("click", function () {
        stopLifecycleAutoPlay();
        activeLifecycleStepIdx = 0;
        if (aboutCatalog) renderAboutLifecycleTab(aboutCatalog);
      });
    }

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
        selectHarnessInModal(btn.getAttribute("data-harness") || "multi");
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
    fetchAboutCatalog();
    initSSE();
  });
})();
