/* TracePoint report: draws the charts from the embedded display data.
   Every element is built with textContent; no value from the data is ever parsed as
   markup, so a target that echoes HTML into a URL or error cannot inject anything. */
(function () {
  "use strict";

  var viewEl = document.getElementById("tp-view");
  if (!viewEl) { return; }
  var view = JSON.parse(viewEl.textContent);

  function cssVar(name) {
    return getComputedStyle(document.documentElement).getPropertyValue(name).trim();
  }
  var fixed = { http: "--s-http", db: "--s-db", redis: "--s-redis" };
  var spare = ["--s-4", "--s-5", "--s-6"];
  function colourFor(name, i) {
    return cssVar(fixed[name] || spare[i % spare.length]);
  }
  function el(tag, text, cls) {
    var e = document.createElement(tag);
    if (text !== undefined && text !== null) { e.textContent = String(text); }
    if (cls) { e.className = cls; }
    return e;
  }
  /* The same resolution rules as the Go renderers, so a number reads the same in the
     tables and in the chart legend. */
  function fmtMS(v) {
    if (v === null || v === undefined) { return "—"; }
    if (v === 0) { return "0"; }
    if (v < 1) { return v.toFixed(2) + "ms"; }
    if (v < 10) { return v.toFixed(1) + "ms"; }
    if (v < 1000) { return v.toFixed(0) + "ms"; }
    if (v < 60000) { return (v / 1000).toFixed(1) + "s"; }
    return (v / 60000).toFixed(1) + "m";
  }
  function fmtS(v) {
    if (v === null || v === undefined) { return "—"; }
    return (Math.round(v * 10) / 10) + "s";
  }
  function fmtNum(v) {
    if (v === null || v === undefined) { return "—"; }
    return Math.abs(v) >= 100 || v === Math.round(v) ? String(Math.round(v)) : v.toPrecision(3);
  }
  function positive(series) {
    return series.map(function (v) { return v !== null && v > 0 ? v : null; });
  }
  function axis(scale, label, side, grid) {
    var a = { scale: scale, label: label, stroke: cssVar("--muted"), grid: { show: grid, stroke: cssVar("--grid") }, ticks: { stroke: cssVar("--grid") } };
    if (side !== undefined) { a.side = side; }
    return a;
  }
  function widthOf(node) {
    return Math.max(280, node.clientWidth || 800);
  }

  var charts = [];
  function track(chart, node, height) {
    charts.push({ chart: chart, node: node, height: height });
    return chart;
  }
  function drop(chart) {
    charts = charts.filter(function (c) { return c.chart !== chart; });
    chart.destroy();
  }

  /* ---- shared timeline ---- */

  var timeline = document.getElementById("timeline-chart");
  var controls = document.getElementById("timeline-controls");
  var state = { q: "p99", lat: "response", inflight: false, log: false, min: null, max: null };
  var main = null;

  function bandsHook(u) {
    var ctx = u.ctx;
    ctx.save();
    view.bands.forEach(function (b) {
      var x0 = u.valToPos(b.from, "x", true);
      var x1 = u.valToPos(b.to, "x", true);
      var left = Math.max(x0, u.bbox.left);
      var right = Math.min(x1, u.bbox.left + u.bbox.width);
      if (right <= left) { return; }
      ctx.fillStyle = cssVar("--band-" + b.kind);
      ctx.fillRect(left, u.bbox.top, right - left, u.bbox.height);
    });
    ctx.restore();
  }

  function buildTimeline() {
    if (!timeline || typeof uPlot === "undefined") { return; }
    if (main) { drop(main); main = null; }
    var data = [view.x];
    var series = [{ label: "time", value: function (u, v) { return fmtS(v); } }];
    view.runners.forEach(function (r, i) {
      var s = r[state.lat][state.q] || [];
      data.push(state.log ? positive(s) : s);
      series.push({
        label: r.name + " " + state.lat + " " + state.q,
        stroke: colourFor(r.name, i), width: 2, scale: "ms", spanGaps: false,
        value: function (u, v) { return fmtMS(v); }
      });
    });
    /* Buckets with too few samples: points only, never joined to the line. */
    view.runners.forEach(function (r, i) {
      var s = (r.sparse || {})[state.lat + "." + state.q] || [];
      if (!s.some(function (v) { return v !== null; })) { return; }
      data.push(state.log ? positive(s) : s);
      series.push({
        label: r.name + " (too few samples)", stroke: colourFor(r.name, i), width: 0, scale: "ms",
        paths: function () { return null; }, points: { show: true, size: 6, width: 1, fill: cssVar("--bg") },
        value: function (u, v) { return fmtMS(v); }
      });
    });
    var axes = [axis("x", "seconds into the run", undefined, true), axis("ms", "milliseconds", undefined, true)];
    if (state.inflight) {
      view.runners.forEach(function (r, i) {
        data.push(r.in_flight || []);
        series.push({
          label: r.name + " in flight", stroke: colourFor(r.name, i), width: 1, dash: [5, 4],
          scale: "n", spanGaps: false, value: function (u, v) { return fmtNum(v); }
        });
      });
      axes.push(axis("n", "in flight", 1, false));
    }
    var opts = {
      width: widthOf(timeline), height: 360,
      scales: { x: { time: false }, ms: state.log ? { distr: 3, log: 10 } : { auto: true, range: function (u, lo, hi) { return [0, hi > 0 ? hi * 1.08 : 1]; } }, n: { auto: true } },
      axes: axes, series: series,
      cursor: { drag: { x: true, y: false } },
      hooks: {
        drawClear: [bandsHook],
        setScale: [function (u, key) {
          if (key === "x") { state.min = u.scales.x.min; state.max = u.scales.x.max; }
        }]
      }
    };
    main = track(new uPlot(opts, data, timeline), timeline, 360);
    if (state.min !== null && state.max !== null) {
      main.setScale("x", { min: state.min, max: state.max });
    }
    fillTimelineTable();
  }

  function zoomTo(from, to) {
    if (!main) { return; }
    var pad = 2 * view.bucket_s;
    state.min = Math.max(view.x[0] || 0, from - pad);
    state.max = to + pad;
    main.setScale("x", { min: state.min, max: state.max });
    timeline.scrollIntoView({ behavior: "smooth", block: "center" });
  }

  function resetZoom() {
    state.min = null;
    state.max = null;
    buildTimeline();
  }

  if (controls) {
    controls.addEventListener("change", function () {
      var f = new FormData(controls);
      state.q = f.get("q") || "p99";
      state.lat = f.get("lat") || "response";
      state.inflight = f.get("inflight") !== null;
      state.log = f.get("log") !== null;
      buildTimeline();
    });
    controls.addEventListener("submit", function (e) { e.preventDefault(); });
  }
  var reset = document.getElementById("timeline-reset");
  if (reset) { reset.addEventListener("click", resetZoom); }
  Array.prototype.forEach.call(document.querySelectorAll("button.zoom"), function (b) {
    b.addEventListener("click", function () {
      var from = Number(b.getAttribute("data-from"));
      var to = Number(b.getAttribute("data-to"));
      zoomTo(from * view.bucket_s, (to + 1) * view.bucket_s);
    });
  });

  /* The timeline as a table, for keyboard and screen-reader users, built when opened. */
  var tableDetails = document.getElementById("timeline-table");
  var tableBody = document.getElementById("timeline-table-body");
  function fillTimelineTable() {
    if (!tableDetails || !tableDetails.open || !tableBody) { return; }
    while (tableBody.firstChild) { tableBody.removeChild(tableBody.firstChild); }
    var table = el("table");
    table.appendChild(el("caption", state.lat + " time " + state.q + " per " + (view.grouped > 1 ? view.grouped + " buckets" : "bucket")));
    var head = el("tr");
    head.appendChild(el("th", "time", "num"));
    view.runners.forEach(function (r) { head.appendChild(el("th", r.name, "num")); });
    var thead = el("thead");
    thead.appendChild(head);
    table.appendChild(thead);
    var tbody = el("tbody");
    view.x.forEach(function (x, i) {
      var tr = el("tr");
      tr.appendChild(el("td", fmtS(x), "num"));
      view.runners.forEach(function (r) {
        var s = r[state.lat][state.q] || [];
        tr.appendChild(el("td", fmtMS(s[i]), "num"));
      });
      tbody.appendChild(tr);
    });
    table.appendChild(tbody);
    tableBody.appendChild(table);
  }
  if (tableDetails) { tableDetails.addEventListener("toggle", fillTimelineTable); }

  /* ---- telemetry ---- */

  var telemetryBySource = {};
  (view.telemetry || []).forEach(function (t) { telemetryBySource[t.source] = t; });
  var telemetryCharts = {};

  function buildTelemetry(select) {
    var source = select.getAttribute("data-source");
    var t = telemetryBySource[source];
    var node = document.querySelector('[data-telemetry="' + source + '"]');
    if (!t || !node || typeof uPlot === "undefined") { return; }
    if (telemetryCharts[source]) { drop(telemetryCharts[source]); }
    var metric = select.value;
    var opts = {
      width: widthOf(node), height: 220,
      scales: { x: { time: false } },
      axes: [axis("x", "seconds into the run", undefined, true), axis("y", metric, undefined, true)],
      series: [
        { label: "time", value: function (u, v) { return fmtS(v); } },
        { label: metric, stroke: cssVar("--accent"), width: 2, spanGaps: false, value: function (u, v) { return fmtNum(v); } }
      ],
      hooks: { drawClear: [bandsHook] }
    };
    telemetryCharts[source] = track(new uPlot(opts, [t.x, t.metrics[metric] || []], node), node, 220);
  }

  Array.prototype.forEach.call(document.querySelectorAll("select.metric"), function (select) {
    var t = telemetryBySource[select.getAttribute("data-source")];
    if (!t) { select.disabled = true; return; }
    Object.keys(t.metrics).sort().forEach(function (name) {
      select.appendChild(el("option", name));
    });
    select.addEventListener("change", function () { buildTelemetry(select); });
    buildTelemetry(select);
  });

  /* ---- capacity ---- */

  var capNode = document.getElementById("capacity-chart");
  var capChart = null;
  function buildCapacity() {
    var c = view.capacity;
    if (!c || !capNode || typeof uPlot === "undefined") { return; }
    if (capChart) { drop(capChart); }
    var opts = {
      width: widthOf(capNode), height: 320,
      scales: { x: { time: false } },
      axes: [axis("x", c.knob, undefined, true), axis("ms", "p99", undefined, true), axis("rps", "achieved per second", 1, false)],
      series: [
        { label: c.knob, value: function (u, v) { return fmtNum(v); } },
        { label: "p99", stroke: cssVar("--s-db"), width: 2, scale: "ms", points: { show: true, size: 7 }, value: function (u, v) { return fmtMS(v); } },
        { label: "achieved/s", stroke: cssVar("--s-http"), width: 2, scale: "rps", points: { show: true, size: 7 }, value: function (u, v) { return fmtNum(v); } }
      ]
    };
    capChart = track(new uPlot(opts, [c.value, c.p99_ms, c.achieved_rps], capNode), capNode, 320);
  }

  /* ---- export ---- */

  var exportButton = document.getElementById("export");
  var full = document.getElementById("tp-result");
  if (exportButton && full) {
    exportButton.addEventListener("click", function () {
      var blob = new Blob([full.textContent], { type: "application/json" });
      var a = document.createElement("a");
      a.href = URL.createObjectURL(blob);
      a.download = "result-" + (view.run_id || "run") + ".json";
      document.body.appendChild(a);
      a.click();
      document.body.removeChild(a);
      setTimeout(function () { URL.revokeObjectURL(a.href); }, 1000);
    });
  }

  /* ---- layout and theme ---- */

  var resizeTimer = null;
  window.addEventListener("resize", function () {
    clearTimeout(resizeTimer);
    resizeTimer = setTimeout(function () {
      charts.forEach(function (c) { c.chart.setSize({ width: widthOf(c.node), height: c.height }); });
    }, 120);
  });
  function rebuildAll() {
    buildTimeline();
    Array.prototype.forEach.call(document.querySelectorAll("select.metric"), buildTelemetry);
    buildCapacity();
  }
  if (window.matchMedia) {
    var mq = window.matchMedia("(prefers-color-scheme: dark)");
    if (mq.addEventListener) { mq.addEventListener("change", rebuildAll); }
  }

  buildTimeline();
  buildCapacity();
}());
