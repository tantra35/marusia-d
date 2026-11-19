#!/usr/bin/env python3
"""SSDP-поиск рендерера unicast M-SEARCH + SOAP RenderingControl SetVolume/GetVolume."""
import socket, sys, re, time

BOARD = "192.168.88.107"

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

def http(method, url, headers, body=b"", timeout=6):
    # без внешних зависимостей: сырой сокет
    import urllib.request
    req = urllib.request.Request(url, data=body, method=method)
    for k, v in headers.items():
        req.add_header(k, v)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return r.status, r.read()
    except urllib.error.HTTPError as e:
        return e.code, e.read()

if __name__ == "__main__":
    action = sys.argv[1] if len(sys.argv) > 1 else "desc"
    loc = msdesc()
    print("LOCATION:", loc)
    if not loc:
        sys.exit(1)
    st, body = http("GET", loc, {})
    txt = body.decode(errors="replace")
    ctl = dict(re.findall(r'<controlURL>([^<]+)</controlURL>', txt)) if False else None
    svcs = re.findall(r'<service>.*?<serviceType>([^<]+)</serviceType>.*?<controlURL>([^<]+)</controlURL>', txt, re.S)
    print("services:", svcs)
    base = loc.split("/uuid-")[0]  # controlURL уже содержит /uuid-.../
    rc = next((base + u for t, u in svcs if "RenderingControl" in t), None)
    avt = next((base + u for t, u in svcs if "AVTransport" in t), None)
    print("RC:", rc, "AVT:", avt)

    def soap(url, service, action_, args):
        body = ('<?xml version="1.0" encoding="utf-8"?>'
                '<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" '
                's:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/">'
                '<s:Body><u:%s xmlns:u="%s">%s</u:%s></s:Body></s:Envelope>'
                % (action_, service, args, action_)).encode()
        h = {"SOAPACTION": '"%s#%s"' % (service, action_),
             "Content-Type": "text/xml; charset=utf-8"}
        return http("POST", url, h, body)

    if action == "setvol":
        vol = sys.argv[2]
        st, r = soap(rc, "urn:schemas-upnp-org:service:RenderingControl:1",
                     "SetVolume",
                     '<InstanceID>0</InstanceID><Channel>Master</Channel>'
                     '<DesiredVolume>%s</DesiredVolume>' % vol)
        print("SetVolume", st, r[:300])
    elif action == "getvol":
        st, r = soap(rc, "urn:schemas-upnp-org:service:RenderingControl:1",
                     "GetVolume",
                     '<InstanceID>0</InstanceID><Channel>Master</Channel>')
        print("GetVolume", st, r[:400])
    elif action == "stop":
        st, r = soap(avt, "urn:schemas-upnp-org:service:AVTransport:1",
                     "Stop", '<InstanceID>0</InstanceID>')
        print("Stop", st, r[:300])
    elif action == "seturi":
        st, r = soap(avt, "urn:schemas-upnp-org:service:AVTransport:1",
                     "SetAVTransportURI",
                     '<InstanceID>0</InstanceID><CurrentURI>%s</CurrentURI>'
                     '<CurrentURIMetaData></CurrentURIMetaData>' % sys.argv[2])
        print("SetAVTransportURI", st, r[:200])
    elif action == "play":
        st, r = soap(avt, "urn:schemas-upnp-org:service:AVTransport:1",
                     "Play", '<InstanceID>0</InstanceID><Speed>1</Speed>')
        print("Play", st, r[:300])
    elif action == "pause":
        st, r = soap(avt, "urn:schemas-upnp-org:service:AVTransport:1",
                     "Pause", '<InstanceID>0</InstanceID>')
        print("Pause", st, r[:300])
