const section = document.querySelector(".desk-shell")?.dataset.activeSection || "today";
const moduleName = {
  today: "today.js",
  tasks: "tasks.js",
}[section];

if (moduleName) {
  const moduleURL = new URL(`./${moduleName}`, import.meta.url);
  const version = new URL(import.meta.url).searchParams.get("v");
  if (version) {
    moduleURL.searchParams.set("v", version);
  }
  void import(moduleURL.href);
}
