async function handler(req) {
  const res = await fetch("http://httpbin.org/delay/2");
  //const data = await res.json();

  return JSON.stringify({
    status: 200,
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({
      message: "Waited async with HTTP!",
      method: request.method,
      url: request.url,
      //data,
    }),
  });
}
