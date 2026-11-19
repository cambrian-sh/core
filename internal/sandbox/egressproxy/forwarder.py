# Cambrian egress forwarder (ENG-688): the docker runner's sidecar.
# Listens on 127.0.0.1:PORT inside the server's network namespace (which has
# no other interface) and relays every connection over stdin/stdout to the
# kernel's egress proxy, framed as egressproxy/mux.go describes:
#   type (1 byte) | stream id (uint32 BE) | length (uint32 BE) | payload
import asyncio, os, struct, sys

H = struct.Struct(">cII")
PORT = int(sys.argv[1]) if len(sys.argv) > 1 else 3128


async def main():
    loop = asyncio.get_running_loop()
    rd = asyncio.StreamReader(limit=1 << 20)
    await loop.connect_read_pipe(lambda: asyncio.StreamReaderProtocol(rd), sys.stdin.buffer)
    wt, wp = await loop.connect_write_pipe(asyncio.streams.FlowControlMixin, sys.stdout.buffer)
    out = asyncio.StreamWriter(wt, wp, None, loop)
    conns = {}
    nid = [0]

    async def send(t, i, b=b""):
        out.write(H.pack(t, i, len(b)) + b)
        await out.drain()

    async def handle(cr, cw):
        nid[0] += 1
        i = nid[0]
        conns[i] = cw
        await send(b"O", i)
        try:
            while True:
                b = await cr.read(32768)
                if not b:
                    break
                await send(b"D", i, b)
        except Exception:
            pass
        if conns.pop(i, None) is not None:
            await send(b"C", i)
        cw.close()

    await asyncio.start_server(handle, "127.0.0.1", PORT)
    await send(b"R", 0)
    while True:
        try:
            hdr = await rd.readexactly(H.size)
            t, i, n = H.unpack(hdr)
            if n > 65536:
                break
            b = await rd.readexactly(n) if n else b""
        except (asyncio.IncompleteReadError, ConnectionError):
            break
        cw = conns.get(i)
        if cw is None:
            continue
        if t == b"D":
            try:
                cw.write(b)
                await cw.drain()
            except Exception:
                conns.pop(i, None)
                cw.close()
        elif t == b"C":
            conns.pop(i, None)
            cw.close()
    os._exit(0)


asyncio.run(main())
