// Fault injection for e2e/assert-clean.fault.selftest.ts, loaded with `node --import`. It makes a port that
// was bound on ::1 once (the selftest's own "is every derived port free" check) refuse the next bind on ::1
// with EADDRINUSE: someone else took the address between the check and the real listen. Only the process
// it is loaded into is affected (a CLI flag, not NODE_OPTIONS, so the checker child is not).
import net from "node:net";

const boundBefore = new Set();
const listen = net.Server.prototype.listen;
net.Server.prototype.listen = function (...args) {
  const opts = args[0];
  if (opts && typeof opts === "object" && opts.host === "::1" && opts.port > 0) {
    if (boundBefore.has(opts.port)) {
      const err = Object.assign(new Error(`listen EADDRINUSE: address already in use ::1:${opts.port}`), { code: "EADDRINUSE", syscall: "listen", address: "::1", port: opts.port });
      process.nextTick(() => this.emit("error", err));
      return this;
    }
    boundBefore.add(opts.port);
  }
  return listen.apply(this, args);
};
