async function handler(req) {
  return JSON.stringify({
    status: 200,
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({
      message: "Response with JSON!",
      method: request.method,
      url: request.url,
    }),
  });
}
