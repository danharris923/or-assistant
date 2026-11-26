"""Build or-assistant_<version>_<arch>.deb without dpkg (works on Windows).

Usage: python packaging/build_deb.py <linux-binary> <out-dir> [arch]
A .deb is an ar archive of: debian-binary, control.tar.gz, data.tar.gz.
"""
import gzip, hashlib, io, os, re, sys, tarfile, time

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
binary, out_dir = sys.argv[1], sys.argv[2]
arch = sys.argv[3] if len(sys.argv) > 3 else "amd64"
version = re.search(r'const version = "([^"]+)"', open(os.path.join(ROOT, "main.go")).read()).group(1)
MTIME = int(os.environ.get("SOURCE_DATE_EPOCH", time.time()))


def read(path, text=False):
    data = open(os.path.join(ROOT, path) if not os.path.isabs(path) else path, "rb").read()
    return data.replace(b"\r\n", b"\n") if text else data


COPYRIGHT = b"""Format: https://www.debian.org/doc/packaging-manuals/copyright-format/1.0/
Upstream-Name: or-assistant

Files: *
Copyright: the OR Assistant authors
License: MIT
"""

# (path inside package, content, mode)
files = [
    ("usr/bin/or-assistant", read(binary), 0o755),
    ("usr/bin/or-assistant-launch", read("linux/or-assistant-launch", True), 0o755),
    ("usr/share/applications/or-assistant.desktop", read("linux/or-assistant.desktop", True), 0o644),
    ("usr/share/icons/hicolor/scalable/apps/or-assistant.svg", read("web/icon.svg", True), 0o644),
    ("usr/share/doc/or-assistant/README.md", read("README.md", True), 0o644),
    ("usr/share/doc/or-assistant/copyright", COPYRIGHT, 0o644),
]


def add(tar, name, data=None, mode=0o644):
    info = tarfile.TarInfo("./" + name if name else ".")
    info.uid = info.gid = 0
    info.uname = info.gname = "root"
    info.mtime = MTIME
    if data is None:
        info.type, info.mode = tarfile.DIRTYPE, 0o755
        tar.addfile(info)
    else:
        info.size, info.mode = len(data), mode
        tar.addfile(info, io.BytesIO(data))


def targz(build):
    raw = io.BytesIO()
    with tarfile.open(fileobj=raw, mode="w", format=tarfile.GNU_FORMAT) as tar:
        build(tar)
    out = io.BytesIO()
    with gzip.GzipFile(fileobj=out, mode="wb", mtime=MTIME, compresslevel=9) as gz:
        gz.write(raw.getvalue())
    return out.getvalue()


def data_tar(tar):
    dirs = sorted({"/".join(p.split("/")[:i]) for p, _, _ in files for i in range(1, p.count("/") + 1)})
    add(tar, "")
    for d in dirs:
        add(tar, d)
    for path, data, mode in files:
        add(tar, path, data, mode)


installed_kb = sum((len(d) + 1023) // 1024 for _, d, _ in files)
control = f"""Package: or-assistant
Version: {version}
Architecture: {arch}
Maintainer: OR Assistant <noreply@localhost>
Installed-Size: {installed_kb}
Section: utils
Priority: optional
Suggests: chromium | chromium-browser | google-chrome-stable
Description: local AI assistant using your own OpenRouter key
 A small web-based AI helper that talks directly to OpenRouter (any model)
 and can run commands and edit files on this computer as the current user.
 It refuses to run as root, asks before every command or file change by
 default, and its local port only answers the window opened by its launcher.
""".encode()
md5sums = "".join(f"{hashlib.md5(d).hexdigest()}  {p}\n" for p, d, _ in files).encode()


def control_tar(tar):
    add(tar, "")
    add(tar, "control", control)
    add(tar, "md5sums", md5sums)


def ar_member(name, data):
    header = f"{name:<16}{MTIME:<12}{0:<6}{0:<6}{'100644':<8}{len(data):<10}`\n".encode()
    assert len(header) == 60
    return header + data + (b"\n" if len(data) % 2 else b"")


deb = b"!<arch>\n" + ar_member("debian-binary", b"2.0\n") + \
    ar_member("control.tar.gz", targz(control_tar)) + ar_member("data.tar.gz", targz(data_tar))
out = os.path.join(out_dir, f"or-assistant_{version}_{arch}.deb")
open(out, "wb").write(deb)
print(f"{out}  ({len(deb):,} bytes)")
