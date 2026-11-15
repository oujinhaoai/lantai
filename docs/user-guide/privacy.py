"""Public help privacy heuristics; diagnostics never echo matched contents."""
import html
import ipaddress
import re
from urllib.parse import unquote, urlsplit


_PRIVATE_NETS = tuple(map(ipaddress.IPv4Network, (
    "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16",
)))
_IPV4 = re.compile(r"(?<![\w.])(?:\d{1,3}\.){3}\d{1,3}(?![\w.])")
_MACHINE_PATH = re.compile(
    r"(?<![\w/])(?:[A-Za-z]:/+(?:Users|Documents and Settings)/+"
    r"|/+(?:Users|home|Volumes|mnt|media)/)"
    r"""[^\s<>"'`，。；、)\]}]+""",
    re.IGNORECASE,
)
_SAFE_PATHS = frozenset({
    "C:/Users/Example/lantai-work",
})
_INTERNAL_DOMAIN = re.compile(
    r"(?<![\w.-])(?:[a-z0-9](?:[a-z0-9-]*[a-z0-9])?\.)+"
    r"(?:internal|intranet|local|lan|corp|home\.arpa)"
    r"(?![\w.-])",
    re.IGNORECASE,
)
_LANTAI_TOKEN = re.compile(r"\b(?:ltk|lts)_[A-Za-z0-9_-]{16,}\b")
_KNOWN_SECRET = re.compile(
    r"\b(?:AKIA|ASIA)[A-Z0-9]{16}\b"
    r"|\bgh[pousr]_[A-Za-z0-9]{20,}\b"
    r"|\bgithub_pat_[A-Za-z0-9_]{20,}\b"
    r"|\bsk-(?:[A-Za-z0-9_-]{20,})\b"
    r"|-----BEGIN (?:[A-Z0-9]+ )*PRIVATE KEY-----"
    r"|otpauth://[^\s<>]+"
)
_ASSIGNMENT = re.compile(
    r"""(?ix)
    (?<![\w-])
    (?:password|passwd|pwd|secret|client[_-]?secret|
       api[_-]?key|access[_-]?token|auth[_-]?token|token)
    ["']?\s*[:=]\s*["']?
    ([A-Za-z0-9_./+~=-]{8,})
    """
)
_BEARER = re.compile(
    r"\bBearer[ \t]+([A-Za-z0-9_./+~=-]{8,})", re.IGNORECASE,
)
_PLACEHOLDER = re.compile(
    r"(?:example|placeholder|redacted|changeme|your[_-][a-z0-9_-]+)",
    re.IGNORECASE,
)


# Publication policy, not a global network allowlist. Add a public documentation
# host only after review; instance examples must use reserved example domains.
_DOCUMENTATION_HOSTS = frozenset({"github.com", "learn.chatgpt.com", "caddyserver.com"})
_HOSTNAME = re.compile(
    r"(?<![\w.-])(?:[a-z0-9](?:[a-z0-9-]*[a-z0-9])?\.)+"
    r"(?:com|net|org|io|dev|app|cn|internal|intranet|local|lan|corp|arpa)"
    r"(?![\w.-])", re.IGNORECASE,
)
_URL = re.compile(r"https?://[^\s<>\"'`]+", re.IGNORECASE)


def example_host(host: str) -> bool:
    return (host == "localhost" or host in _DOCUMENTATION_HOSTS
            or host in {"example.com", "example.net", "example.org"}
            or host.endswith((".example", ".test", ".invalid",
                              ".example.com", ".example.net", ".example.org")))


def privacy_issues(text: str) -> tuple[str, ...]:
    """Find generic privacy risks without returning sensitive matches."""
    for _ in range(2):
        text = html.unescape(unquote(text))
    text = text.replace("\\", "/")
    issues = set()

    for match in _MACHINE_PATH.finditer(text):
        path = re.sub(r"/+", "/", match.group()).rstrip(".,;:!?")
        if path not in _SAFE_PATHS:
            issues.add("machine-path")

    for match in _IPV4.finditer(text):
        try:
            address = ipaddress.IPv4Address(match.group())
        except ipaddress.AddressValueError:
            continue
        if any(address in network for network in _PRIVATE_NETS):
            issues.add("rfc1918-address")

    if _INTERNAL_DOMAIN.search(text):
        issues.add("internal-domain")
    hosts = {m.group().lower() for m in _HOSTNAME.finditer(text)}
    for match in _URL.finditer(text):
        try:
            parsed = urlsplit(match.group().rstrip(".,;)"))
            host = parsed.hostname
            if parsed.username is not None or parsed.password is not None:
                issues.add("credential")
        except ValueError:
            issues.add("invalid-endpoint")
            continue
        if host:
            hosts.add(host.lower())
    for host in hosts:
        if example_host(host) or host == "域名":
            continue
        try:
            address = ipaddress.ip_address(host)
        except ValueError:
            # Also catches public-looking deployment domains without encoding
            # a maintainer-specific hostname into the checker itself.
            if not _INTERNAL_DOMAIN.fullmatch(host):
                issues.add("unreviewed-host")
        else:
            if not address.is_loopback:
                issues.add("non-loopback-endpoint")
    if _LANTAI_TOKEN.search(text):
        issues.add("lantai-token")
    if _KNOWN_SECRET.search(text):
        issues.add("credential")
    for pattern in (_ASSIGNMENT, _BEARER):
        if any(not _PLACEHOLDER.fullmatch(m.group(1))
               for m in pattern.finditer(text)):
            issues.add("credential")

    return tuple(sorted(issues))
