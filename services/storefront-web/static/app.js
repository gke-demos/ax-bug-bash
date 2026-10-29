(function () {
  "use strict";

  const state = {
    currency: "USD",
    cart: [{ sku: "sku-desk-mat-xl", quantity: 1 }],
    promoCodes: [],
    flashBundle: false,
    products: [],
    incidents: [],
    activeIncident: null,
  };

  function setTheme(theme) {
    document.documentElement.setAttribute("data-theme", theme);
    const darkBtn = document.getElementById("theme-dark-btn");
    const lightBtn = document.getElementById("theme-light-btn");
    if (darkBtn && lightBtn) {
      darkBtn.classList.toggle("active", theme === "dark");
      lightBtn.classList.toggle("active", theme === "light");
    }
  }

  function renderServiceStrip() {
    const strip = document.getElementById("service-strip");
    if (!strip) return;
    strip.replaceChildren();

    const services = [
      { id: "storefront-web", lang: "Node.js/TS" },
      { id: "checkout-service", lang: "Go" },
      { id: "shipping-service", lang: "Go" },
      { id: "pricing-service", lang: "Python" },
      { id: "inventory-service", lang: "Rust" },
    ];
    const failing = new Set(
      state.activeIncident ? state.activeIncident.implicated_services || [] : []
    );

    services.forEach((svc) => {
      const pill = document.createElement("div");
      const isFailing = failing.has(svc.id);
      pill.className = "service-pill " + (isFailing ? "failing" : "healthy");
      pill.textContent =
        svc.id + " (" + svc.lang + ") · " + (isFailing ? "HTTP 500" : "200 OK");
      strip.appendChild(pill);
    });
  }

  function renderProducts() {
    const grid = document.getElementById("product-grid");
    if (!grid) return;
    grid.replaceChildren();

    state.products.forEach((p) => {
      const card = document.createElement("div");
      card.className = "product-card";

      const meta = document.createElement("div");
      meta.className = "product-meta";
      meta.textContent = p.category + " · " + p.weight_grams + "g · " + p.hazmat_class;

      const title = document.createElement("h3");
      title.textContent = p.name;

      const desc = document.createElement("p");
      desc.className = "muted";
      desc.textContent = p.description;

      const price = document.createElement("div");
      price.className = "product-price";
      price.textContent = "$" + (p.price_usd_cents / 100).toFixed(2) + " USD";

      const btn = document.createElement("button");
      btn.type = "button";
      btn.className = "secondary-btn";
      btn.textContent = "Select Item in Cart";
      btn.addEventListener("click", () => {
        state.cart = [{ sku: p.sku, quantity: p.flash_bundle_bonus_units > 0 ? 2 : 1 }];
        state.flashBundle = p.flash_bundle_bonus_units > 0;
        const bundleToggle = document.getElementById("flash-bundle-toggle");
        if (bundleToggle) bundleToggle.checked = state.flashBundle;
        renderCart();
      });

      card.appendChild(meta);
      card.appendChild(title);
      card.appendChild(desc);
      card.appendChild(price);
      card.appendChild(btn);
      grid.appendChild(card);
    });
  }

  function renderCart() {
    const container = document.getElementById("cart-items");
    if (!container) return;
    container.replaceChildren();

    state.cart.forEach((item) => {
      const row = document.createElement("div");
      row.textContent = item.sku + " × " + item.quantity + " (" + state.currency + ")";
      container.appendChild(row);
    });
  }

  function renderErrorBanner(checkoutErr) {
    const banner = document.getElementById("error-banner");
    if (!banner) return;
    banner.replaceChildren();
    if (!checkoutErr) {
      banner.classList.add("hidden");
      return;
    }
    banner.classList.remove("hidden");

    const heading = document.createElement("strong");
    heading.textContent =
      "HTTP 500 Internal Server Error — " +
      (checkoutErr.incident_id || "INCIDENT") +
      ": " +
      (checkoutErr.error || "");
    banner.appendChild(heading);

    const pre = document.createElement("pre");
    pre.textContent = (checkoutErr.stack_traces || []).join("\n\n");
    banner.appendChild(pre);
  }

  async function submitCheckout() {
    const resBox = document.getElementById("checkout-result");
    const resp = await fetch("/api/cart/checkout", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        items: state.cart,
        currency: state.currency,
        promo_codes: state.promoCodes,
        flash_bundle: state.flashBundle,
      }),
    });
    const data = await resp.json();
    if (resBox) {
      resBox.replaceChildren();
      const msg = document.createElement("p");
      msg.textContent =
        resp.status === 200
          ? "Order Confirmed (" + data.order_id + ") — Total: " + data.charged_minor_units + " " + data.currency
          : "HTTP 500 Failed — Implicated: " + (data.implicated_services || []).join(", ");
      resBox.appendChild(msg);
    }
    renderErrorBanner(resp.status === 200 ? null : data);
  }

  async function refreshState() {
    const [prodRes, incRes] = await Promise.all([
      fetch("/api/products"),
      fetch("/api/incidents"),
    ]);
    const prodData = await prodRes.json();
    const incData = await incRes.json();
    state.products = prodData.products || [];
    state.incidents = incData.scenarios || [];
    state.activeIncident = incData.active_scenario || null;
    renderServiceStrip();
    renderProducts();
    renderCart();
    renderScenarioModalList();
  }

  function renderScenarioModalList() {
    const list = document.getElementById("scenario-list");
    if (!list) return;
    list.replaceChildren();

    state.incidents.forEach((sc) => {
      const row = document.createElement("div");
      row.className = "scenario-item";

      const info = document.createElement("div");
      const title = document.createElement("strong");
      title.textContent = sc.id + " — " + sc.title;
      const sub = document.createElement("div");
      sub.className = "muted";
      sub.textContent = "Implicated: " + (sc.implicated_services || []).join(", ");
      info.appendChild(title);
      info.appendChild(sub);

      const btn = document.createElement("button");
      btn.type = "button";
      btn.className = "cmd-btn";
      btn.textContent = "Activate " + sc.id;
      btn.addEventListener("click", async () => {
        await fetch("/api/incidents/activate", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ incident_id: sc.id }),
        });
        if (sc.trigger_cart) {
          state.cart = sc.trigger_cart.items || state.cart;
          state.currency = sc.trigger_cart.currency || "USD";
          state.promoCodes = sc.trigger_cart.promo_codes || [];
          state.flashBundle = Boolean(sc.trigger_cart.flash_bundle);
        }
        await refreshState();
        await submitCheckout();
      });

      row.appendChild(info);
      row.appendChild(btn);
      list.appendChild(row);
    });
  }

  document.addEventListener("DOMContentLoaded", () => {
    document.getElementById("theme-dark-btn")?.addEventListener("click", () => setTheme("dark"));
    document.getElementById("theme-light-btn")?.addEventListener("click", () => setTheme("light"));
    document.getElementById("checkout-btn")?.addEventListener("click", submitCheckout);

    document.querySelectorAll(".currency-btn").forEach((btn) => {
      btn.addEventListener("click", () => {
        state.currency = btn.getAttribute("data-currency") || "USD";
        document.querySelectorAll(".currency-btn").forEach((b) =>
          b.classList.toggle("active", b === btn)
        );
        renderCart();
      });
    });

    document.querySelectorAll(".promo-pill").forEach((pill) => {
      pill.addEventListener("click", () => {
        const code = pill.getAttribute("data-promo");
        if (!code) return;
        const idx = state.promoCodes.indexOf(code);
        if (idx >= 0) {
          state.promoCodes.splice(idx, 1);
          pill.classList.remove("active");
        } else {
          state.promoCodes.push(code);
          pill.classList.add("active");
        }
      });
    });

    const modal = document.getElementById("incident-modal");
    document.getElementById("open-incident-modal")?.addEventListener("click", () => {
      modal?.classList.remove("hidden");
    });
    document.getElementById("close-incident-modal")?.addEventListener("click", () => {
      modal?.classList.add("hidden");
    });
    window.addEventListener("keydown", (e) => {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k") {
        e.preventDefault();
        modal?.classList.toggle("hidden");
      } else if (e.key === "Escape") {
        modal?.classList.add("hidden");
      }
    });

    document.getElementById("randomize-incident-btn")?.addEventListener("click", async () => {
      const resp = await fetch("/api/incidents/randomize", { method: "POST" });
      const data = await resp.json();
      if (data.active_scenario && data.active_scenario.trigger_cart) {
        state.cart = data.active_scenario.trigger_cart.items;
        state.currency = data.active_scenario.trigger_cart.currency || "USD";
        state.promoCodes = data.active_scenario.trigger_cart.promo_codes || [];
        state.flashBundle = Boolean(data.active_scenario.trigger_cart.flash_bundle);
      }
      await refreshState();
      await submitCheckout();
    });

    document.getElementById("heal-incident-btn")?.addEventListener("click", async () => {
      await fetch("/api/incidents/reset", { method: "POST" });
      await fetch("/api/incidents/heal", { method: "POST" });
      renderErrorBanner(null);
      await refreshState();
    });

    refreshState();
  });
})();
