/* Runs before the chart library, which formats numbers with the browser's language.
   Some Linux locales (C, POSIX) surface as tags Intl rejects, such as "en-US@posix";
   in that one case fall back to en-US rather than draw no charts at all. */
(function () {
  "use strict";
  try {
    new Intl.NumberFormat(navigator.language);
  } catch (e) {
    try { Object.defineProperty(navigator, "language", { value: "en-US", configurable: true }); } catch (ignored) { /* charts fall back to tables */ }
  }
}());
