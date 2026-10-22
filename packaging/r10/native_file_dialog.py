"""Select a public synthetic CA through our own Xvfb native Open dialog only."""
import ctypes as C,json,os,re,sys,time
from pathlib import Path

display=os.environ.get('DISPLAY','')
assert re.fullmatch(r':\d+(?:\.\d+)?',display),'A private Xvfb display is required'
path=Path(sys.argv[1]).resolve();assert path.is_file() and re.fullmatch(r'[A-Za-z0-9_./-]+',str(path))
assert path.read_bytes().startswith(b'-----BEGIN CERTIFICATE-----'),'Only the synthetic public CA certificate may be selected'
x=C.CDLL('libX11.so.6');xt=C.CDLL('libXtst.so.6')
P=C.c_void_p;W=C.c_ulong
x.XOpenDisplay.argtypes=[C.c_char_p];x.XOpenDisplay.restype=P
x.XDefaultRootWindow.argtypes=[P];x.XDefaultRootWindow.restype=W
x.XQueryTree.argtypes=[P,W,C.POINTER(W),C.POINTER(W),C.POINTER(C.POINTER(W)),C.POINTER(C.c_uint)]
x.XFetchName.argtypes=[P,W,C.POINTER(C.c_char_p)];x.XFree.argtypes=[P]
x.XInternAtom.argtypes=[P,C.c_char_p,C.c_int];x.XInternAtom.restype=W
x.XGetWindowProperty.argtypes=[P,W,W,C.c_long,C.c_long,C.c_int,W,C.POINTER(W),C.POINTER(C.c_int),C.POINTER(W),C.POINTER(W),C.POINTER(C.POINTER(C.c_ubyte))]
x.XSetInputFocus.argtypes=[P,W,C.c_int,W];x.XRaiseWindow.argtypes=[P,W];x.XFlush.argtypes=[P]
x.XStringToKeysym.argtypes=[C.c_char_p];x.XStringToKeysym.restype=W
x.XKeysymToKeycode.argtypes=[P,W];x.XKeysymToKeycode.restype=C.c_ubyte
x.XKeycodeToKeysym.argtypes=[P,C.c_ubyte,C.c_int];x.XKeycodeToKeysym.restype=W
xt.XTestFakeKeyEvent.argtypes=[P,C.c_uint,C.c_int,W]
d=x.XOpenDisplay(display.encode());assert d,'Private display unavailable'
root=x.XDefaultRootWindow(d)
titles=set()
def title(w):
    prop=x.XInternAtom(d,b'_NET_WM_NAME',0);actual=W();fmt=C.c_int();count=W();remaining=W();value=C.POINTER(C.c_ubyte)()
    if x.XGetWindowProperty(d,w,prop,0,1024,0,0,C.byref(actual),C.byref(fmt),C.byref(count),C.byref(remaining),C.byref(value))==0 and value:
        result=C.string_at(value,count.value).decode(errors='replace') if fmt.value==8 else '';x.XFree(C.cast(value,P))
        if result:return result
    name=C.c_char_p()
    if not x.XFetchName(d,w,C.byref(name)) or not name.value:return ''
    value=name.value.decode(errors='replace');x.XFree(C.cast(name,P));return value
def find(w,depth=0):
    if depth>5:return None
    label=title(w)
    if label:titles.add(label[:120])
    if re.search(r'(?i)\b(open|choose|select|import|file upload)\b',label):return w,label
    r,p,children,n=W(),W(),C.POINTER(W)(),C.c_uint()
    if not x.XQueryTree(d,w,C.byref(r),C.byref(p),C.byref(children),C.byref(n)):return None
    values=list(children[:n.value]);x.XFree(C.cast(children,P))
    for child in values:
        result=find(child,depth+1)
        if result:return result
cancel='--cancel' in sys.argv
target=None;deadline=time.monotonic()+(1 if cancel else 15)
while target is None and time.monotonic()<deadline:
    target=find(root)
    if target is None:time.sleep(.1)
if target is None and cancel:sys.exit(0)
assert target,'Native certificate Open dialog unavailable; private display titles: '+json.dumps(sorted(titles)[:12])
time.sleep(.5);x.XRaiseWindow(d,target[0]);x.XSetInputFocus(d,target[0],2,0);x.XFlush(d);time.sleep(.2)
def code(name):return x.XKeysymToKeycode(d,x.XStringToKeysym(name.encode()))
def event(key,down):assert xt.XTestFakeKeyEvent(d,key,int(down),0),'Private display XTEST input unavailable'
def tap(name):
    key=code(name);assert key;event(key,True);event(key,False);x.XFlush(d)
if cancel:tap('Escape');time.sleep(.1);tap('Escape');sys.exit(0)
control=code('Control_L');event(control,True);tap('l');event(control,False);x.XFlush(d);time.sleep(.2)
event(control,True);tap('a');event(control,False);x.XFlush(d);time.sleep(.1)
names={'/':'slash','.':'period','-':'minus','_':'underscore'}
for char in str(path):
    symbol=x.XStringToKeysym(names.get(char,char).encode());key=x.XKeysymToKeycode(d,symbol);assert key
    shift=x.XKeycodeToKeysym(d,key,0)!=symbol
    if shift:event(code('Shift_L'),True)
    event(key,True);event(key,False)
    if shift:event(code('Shift_L'),False)
    x.XFlush(d);time.sleep(.01)
tap('Return');time.sleep(.7);titles.clear();remaining=find(root)
print(json.dumps({'native_dialog':target[1],'public_synthetic_certificate_selected':True,'private_display':True,'open_dialog_remaining':bool(remaining),'private_window_titles':sorted(titles)[:12]}))
