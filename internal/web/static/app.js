(function () {
  var root = document.documentElement;
  var app = document.getElementById("app");

  function store(key, value) {
    try {
      if (value === undefined) return localStorage.getItem(key);
      localStorage.setItem(key, value);
    } catch (e) {}
  }

  // Theme toggle
  document.addEventListener("click", function (e) {
    if (!e.target.closest("[data-theme-toggle]")) return;
    var theme = root.getAttribute("data-theme") === "light" ? "dark" : "light";
    root.setAttribute("data-theme", theme);
    store("theme", theme);
    renderCharts(document, true);
  });

  // Sidebar: collapse on desktop, drawer on mobile
  if (app && store("sidebar") === "collapsed") app.classList.add("collapsed");
  document.addEventListener("click", function (e) {
    if (e.target.closest("[data-collapse]")) {
      app.classList.toggle("collapsed");
      store("sidebar", app.classList.contains("collapsed") ? "collapsed" : "open");
      return;
    }
    if (e.target.closest("[data-menu]")) {
      app.classList.toggle("menu-open");
      return;
    }
    if (app && app.classList.contains("menu-open") && !e.target.closest(".sidebar")) {
      app.classList.remove("menu-open");
    }
    // Close the user menu and filter pickers when clicking elsewhere.
    document
      .querySelectorAll("details.user-menu[open], details.picker[open]")
      .forEach(function (d) {
        if (!d.contains(e.target)) d.open = false;
      });
  });

  // Period selector: remembered in a cookie the server reads.
  document.addEventListener("change", function (e) {
    if (!e.target.matches("[data-period]")) return;
    document.cookie = "period=" + e.target.value + "; path=/; max-age=31536000; samesite=lax";
    var url = new URL(location.href);
    url.searchParams.delete("period");
    location.href = url.toString();
  });

  // Dashboard filter pickers: the server applies the filter; this keeps the labels in step.
  document.addEventListener("change", function (e) {
    var picker = e.target.closest("[data-picker]");
    if (!picker || e.target.type !== "checkbox") return;
    var picked = Array.prototype.map.call(picker.querySelectorAll("input:checked"), function (i) {
      return i.value;
    });
    picker.querySelector("[data-picker-value]").textContent =
      picked.length === 0 ? "All" : picked.length === 1 ? picked[0] : picked.length + " selected";
    var form = picker.closest("form");
    var clear = form && form.querySelector("[data-clear]");
    if (clear) clear.hidden = !form.querySelector("[data-picker] input:checked");
  });
  document.addEventListener("input", function (e) {
    if (!e.target.matches("[data-picker-search]")) return;
    var needle = e.target.value.trim().toLowerCase();
    e.target
      .closest("[data-picker]")
      .querySelectorAll(".picker-list .chk")
      .forEach(function (row) {
        row.hidden = needle !== "" && row.textContent.toLowerCase().indexOf(needle) < 0;
      });
  });
  document.addEventListener("keydown", function (e) {
    if (e.key === "Enter" && e.target.matches("[data-picker-search]")) e.preventDefault();
  });

  // Copy buttons
  document.addEventListener("click", function (e) {
    var btn = e.target.closest("[data-copy]");
    if (!btn) return;
    var el = document.querySelector(btn.dataset.copy);
    if (!el || !navigator.clipboard) return;
    navigator.clipboard.writeText(el.textContent.trim()).then(function () {
      var label = btn.textContent;
      btn.textContent = "Copied";
      setTimeout(function () {
        btn.textContent = label;
      }, 1500);
    });
  });

  // Toasts, sent by the server as an HX-Trigger header.
  function toast(kind, message) {
    var box = document.getElementById("toasts");
    if (!box) return;
    var t = document.createElement("div");
    t.className = "toast" + (kind === "error" ? " toast-error" : "");
    t.setAttribute("role", kind === "error" ? "alert" : "status");
    t.textContent = message;
    box.appendChild(t);
    setTimeout(
      function () {
        t.remove();
      },
      kind === "error" ? 8000 : 4000,
    );
  }
  document.body.addEventListener("toast", function (e) {
    toast(e.detail.kind, e.detail.message);
  });
  document.addEventListener("htmx:responseError", function (e) {
    if (e.detail.xhr.getResponseHeader("HX-Trigger")) return; // the server already sent a toast
    toast("error", e.detail.xhr.responseText || "Request failed.");
  });

  // Charts: specs live in data-chart; colours come from the theme's CSS variables.
  var charts = new Map();
  function cssVar(name) {
    return getComputedStyle(root).getPropertyValue(name).trim();
  }
  function alpha(color, a) {
    var m = /^#([0-9a-f]{6})$/i.exec(color);
    if (!m) return color;
    var n = parseInt(m[1], 16);
    return "rgba(" + (n >> 16) + "," + ((n >> 8) & 255) + "," + (n & 255) + "," + a + ")";
  }

  function build(canvas) {
    var spec = JSON.parse(canvas.dataset.chart);
    var text = cssVar("--subtext0"),
      grid = cssVar("--surface0");
    var font = { family: getComputedStyle(document.body).fontFamily, size: 11 };
    var legend = {
      labels: { color: text, font: font, boxWidth: 8, boxHeight: 8, usePointStyle: true },
    };
    var tooltip = {
      backgroundColor: cssVar("--crust"),
      titleColor: cssVar("--text"),
      bodyColor: cssVar("--text"),
      borderColor: grid,
      borderWidth: 1,
      titleFont: font,
      bodyFont: font,
    };
    var animation = matchMedia("(prefers-reduced-motion: reduce)").matches
      ? false
      : { duration: 300 };

    if (spec.kind === "doughnut") {
      return new Chart(canvas, {
        type: "doughnut",
        data: {
          labels: spec.labels,
          datasets: [
            {
              data: spec.series.map(function (s) {
                return s.data[0];
              }),
              backgroundColor: spec.series.map(function (s) {
                return cssVar(s.color);
              }),
              borderColor: cssVar("--base"),
              borderWidth: 3,
            },
          ],
        },
        options: {
          responsive: true,
          maintainAspectRatio: false,
          animation: animation,
          cutout: "68%",
          plugins: { legend: Object.assign({ position: "bottom" }, legend), tooltip: tooltip },
        },
      });
    }

    var scales = {
      x: {
        ticks: { color: text, font: font, maxRotation: 0, autoSkipPadding: 16 },
        grid: { color: grid, drawTicks: false },
        border: { color: grid },
      },
      y: {
        beginAtZero: true,
        ticks: { color: text, font: font, precision: 0 },
        grid: { color: grid, drawTicks: false },
        border: { display: false },
      },
    };
    var options = {
      responsive: true,
      maintainAspectRatio: false,
      animation: animation,
      scales: scales,
      plugins: { legend: legend, tooltip: tooltip },
    };

    if (spec.kind === "bars") {
      return new Chart(canvas, {
        type: "bar",
        data: {
          labels: spec.labels,
          datasets: spec.series.map(function (s) {
            return {
              label: s.name,
              data: s.data,
              backgroundColor: alpha(cssVar(s.color), 0.7),
              borderRadius: 3,
              maxBarThickness: 18,
            };
          }),
        },
        options: options,
      });
    }

    options.interaction = { mode: "index", intersect: false };
    return new Chart(canvas, {
      type: "line",
      data: {
        labels: spec.labels,
        datasets: spec.series.map(function (s) {
          var c = cssVar(s.color);
          return {
            label: s.name,
            data: s.data,
            borderColor: c,
            backgroundColor: alpha(c, 0.12),
            fill: true,
            tension: 0.35,
            pointRadius: 0,
            pointHoverRadius: 4,
            borderWidth: 2,
          };
        }),
      },
      options: options,
    });
  }

  // update replaces a chart's data in place, without animation.
  function update(chart, spec) {
    chart.data.labels = spec.labels;
    if (spec.kind === "doughnut") {
      chart.data.datasets[0].data = spec.series.map(function (s) {
        return s.data[0];
      });
    } else {
      spec.series.forEach(function (s, i) {
        if (chart.data.datasets[i]) chart.data.datasets[i].data = s.data;
      });
    }
    chart.update("none");
  }

  // renderCharts draws the charts in scope. Charts are keyed by data-chart-id, so when
  // a live refresh re-renders a region, the existing chart (and its canvas) is kept
  // and only its data changes. Rebuilding would blank the canvas and replay the
  // draw animation on every refresh. rebuild=true redraws everything (theme change).
  function renderCharts(scope, rebuild) {
    if (!window.Chart) return;
    if (rebuild) {
      charts.forEach(function (c) {
        c.chart.destroy();
      });
      charts.clear();
    }
    scope.querySelectorAll("canvas[data-chart]").forEach(function (canvas) {
      var id = canvas.dataset.chartId || canvas.dataset.chart;
      var prev = charts.get(id);
      if (prev && prev.canvas === canvas) return;
      if (prev) {
        var spec = canvas.dataset.chart;
        canvas.replaceWith(prev.canvas);
        if (spec !== prev.spec) {
          prev.spec = spec;
          prev.canvas.dataset.chart = spec;
          update(prev.chart, JSON.parse(spec));
        }
        return;
      }
      try {
        charts.set(id, { chart: build(canvas), canvas: canvas, spec: canvas.dataset.chart });
      } catch (err) {
        console.error("chart", err);
      }
    });
    charts.forEach(function (c, id) {
      if (!document.body.contains(c.canvas)) {
        c.chart.destroy();
        charts.delete(id);
      }
    });
  }
  window.addEventListener("load", function () {
    renderCharts(document);
  });

  // Keep expanded runs and jobs expanded, and loaded logs in place, when a live region re-renders.
  var liveRegions = ["feed", "dash", "run-detail"];
  var open = new Set();
  var logs = new Map();
  var logScroll = new Map();
  document.addEventListener(
    "toggle",
    function (e) {
      var d = e.target;
      if (!(d instanceof HTMLDetailsElement) || !d.id) return;
      if (!d.classList.contains("run") && !d.classList.contains("job")) return;
      if (d.open) open.add(d.id);
      else open.delete(d.id);
    },
    true,
  );

  function jumpToError(log) {
    var line = log.querySelector(".l-error");
    var pre = log.querySelector(".log-pre");
    if (line && pre) pre.scrollTop = line.offsetTop - pre.offsetTop - 48;
  }

  document.addEventListener("htmx:beforeSwap", function (e) {
    if (liveRegions.indexOf(e.detail.target.id) < 0) return;
    logScroll.clear();
    e.detail.target.querySelectorAll(".log-slot .log-pre").forEach(function (pre) {
      logScroll.set(pre.closest(".log-slot").id, pre.scrollTop);
    });
  });

  document.addEventListener("htmx:afterSwap", function (e) {
    var target = e.detail.target;
    if (liveRegions.indexOf(target.id) >= 0) {
      open.forEach(function (id) {
        var d = document.getElementById(id);
        if (d) d.open = true;
      });
      logs.forEach(function (html, id) {
        var slot = document.getElementById(id);
        if (slot && !slot.innerHTML.trim()) {
          slot.innerHTML = html;
          htmx.process(slot);
          var pre = slot.querySelector(".log-pre");
          if (pre && logScroll.has(id)) pre.scrollTop = logScroll.get(id);
        }
      });
      renderCharts(target);
      return;
    }
    if (target.classList && target.classList.contains("log-slot")) {
      logs.set(target.id, target.innerHTML);
      var log = target.querySelector(".log");
      if (!log) return;
      var pre = log.querySelector(".log-pre");
      if (log.querySelector(".l-error")) jumpToError(log);
      else if (pre) pre.scrollTop = pre.scrollHeight;
    }
  });

  document.addEventListener("click", function (e) {
    if (!e.target.closest("[data-jump-error]")) return;
    jumpToError(e.target.closest(".log"));
  });

  // Live connection indicator
  function setLive(state, text) {
    var el = document.getElementById("live");
    if (!el) return;
    el.classList.toggle("on", state === "on");
    el.classList.toggle("off", state === "off");
    el.querySelector(".live-text").textContent = text;
  }
  document.addEventListener("htmx:sseOpen", function () {
    setLive("on", "Live");
  });
  document.addEventListener("htmx:sseError", function () {
    setLive("off", "Reconnecting");
  });
})();
