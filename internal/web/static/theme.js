// Runs before first paint so the saved theme never flashes. Dark is the default.
(function () {
  var theme = "dark";
  try {
    if (localStorage.getItem("theme") === "light") theme = "light";
  } catch (e) {}
  document.documentElement.setAttribute("data-theme", theme);
})();
