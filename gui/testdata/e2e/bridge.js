// Stands in for the Wails runtime in TestDesktopUI: window.go.main.App calls
// go to the real Go backend over HTTP, and backend events arrive through
// server-sent events. As in Wails, a call resolves only after the events it
// emitted have been delivered.
'use strict';

(() => {
  const listeners = {};
  let received = 0;
  let dropHandler = null;

  const source = new EventSource('/test/events');
  source.onmessage = (msg) => {
    const ev = JSON.parse(msg.data);
    received++;
    (listeners[ev.name] || []).slice().forEach((cb) => cb(...(ev.data || [])));
  };

  async function invoke(method, args) {
    const res = await fetch('/test/call', { method: 'POST', body: JSON.stringify({ method, args }) });
    const reply = await res.json();
    while (received < reply.seq) await new Promise((r) => setTimeout(r, 5));
    if ('error' in reply) throw new Error(reply.error);
    return reply.result;
  }

  window.go = { main: { App: new Proxy({}, { get: (_, method) => (...args) => invoke(method, args) }) } };

  window.runtime = {
    EventsOn(name, cb) {
      (listeners[name] = listeners[name] || []).push(cb);
      return () => { listeners[name] = listeners[name].filter((f) => f !== cb); };
    },
    OnFileDrop(cb) { dropHandler = cb; },
    ClipboardSetText: async () => true,
  };

  // Used by the driver to simulate dropping folders onto the window.
  window.testBridge = { drop: (paths) => dropHandler && dropHandler(0, 0, paths) };
})();
