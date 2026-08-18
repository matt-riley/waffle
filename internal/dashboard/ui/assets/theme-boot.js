// theme-boot.js — no modules, no inline handlers, no DOM writes except <html>.
(function () {
  var key = "waffle.desk.theme";
  var allowed = { system: 1, light: 1, dark: 1 };
  var stored = "";
  try { stored = localStorage.getItem(key) || "system"; } catch (e) { stored = "system"; }
  if (!Object.prototype.hasOwnProperty.call(allowed, stored)) stored = "system";
  var dark = window.matchMedia("(prefers-color-scheme: dark)").matches;
  var theme = stored === "system" ? (dark ? "dark" : "light") : stored;
  document.documentElement.setAttribute("data-theme", theme);
  document.documentElement.setAttribute("data-theme-preference", stored);
})();
