const desk = document.querySelector(".desk-shell");

if (desk) {
  const requestToken = document.body.dataset.requestToken;
  desk.dataset.ready = requestToken ? "true" : "false";
}
