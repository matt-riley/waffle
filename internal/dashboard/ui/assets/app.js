const moduleURL = new URL("./today.js", import.meta.url);
const version = new URL(import.meta.url).searchParams.get("v");

if (version) {
  moduleURL.searchParams.set("v", version);
}

void import(moduleURL.href);
