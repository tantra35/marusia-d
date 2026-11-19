#!/usr/bin/env python3
"""Подписка на RenderingControl NOTIFY от upmpdcli: печатаем каждый LastChange."""
import socket, sys, re, time, threading
import urllib.request
import http.server

BOARD = "192.168.88.107"
CALLBACK_PORT = 8899
TIMEOUT = int(sys.argv[1]) if len(sys.argv) > 1 else 60

def msdesc():
    s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM, socket.IPPROTO_UDP)
    s.settimeout(4)
    msg = ('M-SEARCH * HTTP/1.1\r\nHOST: %s:1900\r\nMAN: "ssdp:discover"\r\n'
           'MX: 2\r\nST: urn:schemas-upnp-org:device:MediaRenderer:1\r\n\r\n' % BOARD)
    s.sendto(msg.encode(), (BOARD, 1900))
    loc = None
    try:
        while True:
            d, _ = s.recvfrom(4096)
            m = re.search(r'LOCATION:\s*(\S+)', d.decode(errors='replace'), re.I)
            if m:
                loc = m.group(1)
                break
    except socket.timeout:
        pass
    s.close()
    return loc

events = []

class CB(http.server.BaseHTTPRequestHandler):
    def do_NOTIFY(self):
        ln = int(self.headers.get("Content-Length", 0))
        body = self.rfile.read(ln).decode(errors="replace")
        body = body.replace("&lt;", "<").replace("&gt;", ">").replace("&quot;", '"')
        vols = re.findall(r'<Volume[^>]*val="(\d+)"[^>]*/>', body)
        mutes = re.findall(r'<Mute[^>]*val="(\d+)"[^>]*/>', body)
        ts_state = re.findall(r'<TransportState val="([^"]+)"', body)
        ts = time.strftime("%H:%M:%S")
        extra = f" TransportState={ts_state}" if ts_state else ""
        print(f"{ts} NOTIFY Volume={vols} Mute={mutes}{extra}", flush=True)
        events.append((ts, vols))
        self.send_response(200)
        self.end_headers()
    def do_GET(self):
        self.send_response(200); self.end_headers()
    def log_message(self, *a):
        pass

loc = msdesc()
print("LOCATION:", loc)
if not loc:
    sys.exit(1)
st, body = urllib.request.urlopen(loc, timeout=5).read(), None
import urllib.request as ur
txt = ur.urlopen(loc, timeout=5).read().decode(errors="replace")
svcs = re.findall(r'<service>.*?<serviceType>([^<]+)</serviceType>.*?<eventSubURL>([^<]+)</eventSubURL>', txt, re.S)
base = loc.split("/uuid-")[0]
rc_sub = next((base + u for t, u in svcs if "RenderingControl" in t), None)
avt_sub = next((base + u for t, u in svcs if "AVTransport" in t), None)
print("RC eventSubURL:", rc_sub)
print("AVT eventSubURL:", avt_sub)

srv = http.server.HTTPServer(("0.0.0.0", CALLBACK_PORT), CB)
threading.Thread(target=srv.serve_forever, daemon=True).start()

cb = "http://%s:%d/cb" % (socket.gethostbyname(socket.gethostname()), CALLBACK_PORT)
# найдём наш IP в LAN — через обычный сокет к плате
probe = socket.create_connection((BOARD, 49152), timeout=3)
cb = "http://%s:%d/cb" % (probe.getsockname()[0], CALLBACK_PORT)
probe.close()
print("callback:", cb)

req = urllib.request.Request("http://" + BOARD + ":49152" + rc_sub.replace("http://" + BOARD + ":49152", ""), method="SUBSCRIBE") \
    if False else None
# сырой SUBSCRIBE (urllib не знает метод) — через http.client
import http.client
conn = http.client.HTTPConnection(BOARD, 49152, timeout=5)
path = rc_sub[len("http://%s:49152" % BOARD):]
conn.request("SUBSCRIBE", path, headers={
    "CALLBACK": "<%s>" % cb, "NT": "upnp:event", "TIMEOUT": "Second-%d" % (TIMEOUT + 60)})
r = conn.getresponse()
print("SUBSCRIBE RC:", r.status, "sid:", r.getheader("SID"))
conn.close()

if avt_sub:
    conn = http.client.HTTPConnection(BOARD, 49152, timeout=5)
    path = avt_sub[len("http://%s:49152" % BOARD):]
    conn.request("SUBSCRIBE", path, headers={
        "CALLBACK": "<%s>" % cb, "NT": "upnp:event", "TIMEOUT": "Second-%d" % (TIMEOUT + 60)})
    r = conn.getresponse()
    print("SUBSCRIBE AVT:", r.status, "sid:", r.getheader("SID"))
    conn.close()

t0 = time.time()
try:
    while time.time() - t0 < TIMEOUT:
        time.sleep(0.5)
except KeyboardInterrupt:
    pass
print("событий получено:", len(events))
