/* TracePoint comparison: overlays each runner's p99 in the two runs. Every element is
   built with textContent; nothing from the data is ever parsed as markup. */
(function () {
  "use strict";

  var viewEl = document.getElementById("tp-view");
  var host = document.getElementById("overlay");
  if (!viewEl || !host || typeof uPlot === "undefined") { return; }
  var view = JSON.parse(viewEl.textContent);

  function cssVar(name) {
    return getComputedStyle(document.documentElement).getPropertyValue(name).trim();
  }
  function el(tag, text, cls) {
    var e = document.createElement(tag);
    if (text !== undefined && text !== null) { e.textContent = String(text); }
    if (cls) { e.className = cls; }
    return e;
  }
  function fmtMS(v) {
    if (v === null || v === undefined) { return "—"; }
    if (v < 1) { return v.toFixed(2) + "ms"; }
    if (v < 10) { return v.toFixed(1) + "ms"; }
    if (v < 1000) { return v.toFixed(0) + "ms"; }
    return (v / 1000).toFixed(1) + "s";
  }
  /* The two runs sample different instants, so they share one x axis built from the
     union of both, each series null where it has no point. */
  function merge(a, b) {
    var xs = {};
    a.x.forEach(function (x, i) { xs[x] = [a.y[i], null]; });
    b.x.forEach(function (x, i) { xs[x] = [xs[x] ? xs[x][0] : null, b.y[i]]; });
    var keys = Object.keys(xs).map(Number).sort(function (p, q) { return p - q; });
    return [keys, keys.map(function (k) { return xs[k][0]; }), keys.map(function (k) { return xs[k][1]; })];
  }

  var charts = [];
  view.runners.forEach(function (r) {
    var fig = el("figure", null, "chart");
    fig.appendChild(el("figcaption", r.name + ": p99 response time, baseline and current, each from its own start"));
    var box = el("div");
    fig.appendChild(box);
    host.appendChild(fig);
    var data = merge(r.baseline || { x: [], y: [] }, r.current || { x: [], y: [] });
    var chart = new uPlot({
      width: Math.max(280, host.clientWidth || 800), height: 260,
      scales: { x: { time: false } },
      axes: [
        { label: "seconds from start", stroke: cssVar("--muted"), grid: { stroke: cssVar("--grid") } },
        { label: "p99", stroke: cssVar("--muted"), grid: { stroke: cssVar("--grid") }, values: function (u, vs) { return vs.map(fmtMS); } }
      ],
      series: [
        {},
        { label: "baseline", stroke: cssVar("--muted"), width: 2, dash: [6, 4], spanGaps: false, value: function (u, v) { return fmtMS(v); } },
        { label: "current", stroke: cssVar("--s-http"), width: 2, spanGaps: false, value: function (u, v) { return fmtMS(v); } }
      ]
    }, data, box);
    charts.push({ chart: chart, node: host });
  });
  window.addEventListener("resize", function () {
    charts.forEach(function (c) { c.chart.setSize({ width: Math.max(280, c.node.clientWidth || 800), height: 260 }); });
  });
})();
