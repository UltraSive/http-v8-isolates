async function fetch(request) {
  await fetch("https://httpbin.org/delay/5");

  return JSON.stringify({
    status: 200,
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({
      message: "Waited async with HTTP!",
      method: request.method,
      url: request.url,
    }),
  });
}