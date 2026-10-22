"""Session-private FileChooser provider: select one explicitly pinned synthetic CA.

This implements the documented desktop file-selection protocol, not certificate
verification. Chrome imports and validates the certificate normally afterwards.
No host/session portal is replaced; a private dbus-run-session is mandatory.
"""
import hashlib,json,os,re,signal,sys
from pathlib import Path
import dbus,dbus.service
from dbus.mainloop.glib import DBusGMainLoop
from gi.repository import GLib

ca=Path(sys.argv[1]).resolve();ready=Path(sys.argv[2]);report=Path(sys.argv[3])
address=os.environ.get('DBUS_SESSION_BUS_ADDRESS','')
assert address.startswith(('unix:path=/tmp/dbus-','unix:abstract=/tmp/dbus-')),'Private D-Bus session required'
assert str(ca).startswith(('/tmp/lantai-safe-browser-','/tmp/r10-native-ca-probe-'))
body=ca.read_bytes();assert body.startswith(b'-----BEGIN CERTIFICATE-----') and b'PRIVATE KEY' not in body
state={'protocol':'org.freedesktop.portal.FileChooser','scope':'private test-session only',
    'synthetic_CA_sha256':hashlib.sha256(body).hexdigest(),'requests':0,
    'host_portal_or_trust_store_modified':False,'certificate_validation_overridden':False}
def save():report.write_text(json.dumps(state)+'\n')
DBusGMainLoop(set_as_default=True);bus=dbus.SessionBus();loop=GLib.MainLoop()
name=dbus.service.BusName('org.freedesktop.portal.Desktop',bus=bus,do_not_queue=True,allow_replacement=False)
requests=[]
class Request(dbus.service.Object):
    @dbus.service.signal('org.freedesktop.portal.Request',signature='ua{sv}')
    def Response(self,code,result):pass
    @dbus.service.method('org.freedesktop.portal.Request',in_signature='',out_signature='')
    def Close(self):self.remove_from_connection()
class Portal(dbus.service.Object):
    @dbus.service.method('org.freedesktop.DBus.Properties',in_signature='ss',out_signature='v')
    def Get(self,interface,prop):
        if interface=='org.freedesktop.portal.FileChooser' and prop=='version':return dbus.UInt32(4)
        raise dbus.exceptions.DBusException('Unknown property')
    @dbus.service.method('org.freedesktop.DBus.Properties',in_signature='s',out_signature='a{sv}')
    def GetAll(self,interface):return {'version':dbus.UInt32(4)} if interface=='org.freedesktop.portal.FileChooser' else {}
    @dbus.service.method('org.freedesktop.portal.FileChooser',in_signature='ssa{sv}',out_signature='o',sender_keyword='sender')
    def OpenFile(self,parent,title,options,sender=None):
        token=str(options.get('handle_token','selected_ca'));assert re.fullmatch(r'[A-Za-z0-9_]+',token)
        path='/org/freedesktop/portal/desktop/request/'+sender[1:].replace('.','_')+'/'+token
        request=Request(bus,path);requests.append(request);state['requests']+=1;save()
        def respond():
            if state['requests']==1:request.Response(dbus.UInt32(0),{'uris':dbus.Array([ca.as_uri()],signature='s')})
            else:request.Response(dbus.UInt32(2),{})
            return False
        GLib.timeout_add(150,respond)
        return dbus.ObjectPath(path)
portal=Portal(bus,'/org/freedesktop/portal/desktop');save();ready.write_text('ready\n')
signal.signal(signal.SIGTERM,lambda *_:loop.quit())
try:loop.run()
finally:save()
