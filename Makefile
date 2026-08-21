# WaveControl build and installation workflow.
#
# This file intentionally uses the common GNU make/BSD make (pmake) subset so the same
# targets work with GNU make on Linux and BSD make on OpenBSD. Do not add GNU
# make functions, pattern-specific variables, or shell-specific install flags.

GO?=go
GOFMT?=gofmt
CGO_ENABLED?=0
GOFLAGS?=
GO_BUILD_FLAGS?=-trimpath
BUILD_DIR?=build
BINARY?=${BUILD_DIR}/wavecontrol
TARGET_ARCH?=amd64

DESTDIR?=
PREFIX?=/usr/local
BINDIR?=${PREFIX}/bin
SHAREDIR?=${PREFIX}/share/wavecontrol
DOCDIR?=${PREFIX}/share/doc/wavecontrol
SYSCONFDIR?=/etc
CONFDIR?=${SYSCONFDIR}/wavecontrol
ENVFILE?=${CONFDIR}/wavecontrol.env
RUNDIR?=/var/wavecontrol
SYSTEMD_UNITDIR?=${SYSCONFDIR}/systemd/system
RCDIR?=${SYSCONFDIR}/rc.d
RUN_USER?=_wavecontrol
RUN_GROUP?=_wavecontrol
INSTALL_OS?=

.NOTPARALLEL:
.PHONY: all build test vet fmt fmt-check check env env-check run clean \
	cross-linux cross-openbsd cross-windows install install-user \
	install-files install-config install-service enable start restart status \
	stop uninstall help print-config

all: build

build:
	@mkdir -p "${BUILD_DIR}"
	@echo "==> building ${BINARY}"
	@CGO_ENABLED=${CGO_ENABLED} ${GO} build ${GOFLAGS} ${GO_BUILD_FLAGS} -o "${BINARY}" ./cmd/server

test:
	@echo "==> running Go tests"
	@CGO_ENABLED=${CGO_ENABLED} ${GO} test ${GOFLAGS} ./...

vet:
	@echo "==> running go vet"
	@CGO_ENABLED=${CGO_ENABLED} ${GO} vet ${GOFLAGS} ./...

fmt:
	@echo "==> formatting Go sources"
	@${GOFMT} -w $$(find cmd internal -type f -name '*.go' -print)

fmt-check:
	@echo "==> checking Go formatting"
	@bad=`${GOFMT} -l $$(find cmd internal -type f -name '*.go' -print)`; \
	if [ -n "$$bad" ]; then \
		echo "The following files require gofmt:" >&2; \
		echo "$$bad" >&2; \
		exit 1; \
	fi

env-check:
	@cmp -s wavecontrol.env.example systemd/wavecontrol.env.example || { \
		echo "systemd/wavecontrol.env.example is out of sync with wavecontrol.env.example" >&2; \
		exit 1; \
	}
	@cmp -s wavecontrol.env.example windows/wavecontrol.env.example || { \
		echo "windows/wavecontrol.env.example is out of sync with wavecontrol.env.example" >&2; \
		exit 1; \
	}

check: fmt-check env-check test vet

env:
	@if [ -f wavecontrol.env ]; then \
		chmod 0600 wavecontrol.env; \
		echo "wavecontrol.env already exists; leaving its contents unchanged"; \
	else \
		cp wavecontrol.env.example wavecontrol.env; \
		chmod 0600 wavecontrol.env; \
		echo "created ./wavecontrol.env from the sample; edit it before running WaveControl"; \
	fi

run: build env
	@root=`pwd`; \
	if [ `id -u` -eq 0 ]; then \
		echo "make run is for an unprivileged development account; do not run it as root" >&2; \
		exit 1; \
	fi; \
	mkdir -p "$$root/.wavecontrol/firmware" "$$root/.wavecontrol/backups"; \
	set -a; . "$$root/wavecontrol.env"; set +a; \
	exec "$$root/${BINARY}" -d -workdir "$$root/.wavecontrol" -webroot "$$root/web"

cross-linux:
	@mkdir -p "${BUILD_DIR}/linux-${TARGET_ARCH}"
	@echo "==> cross-building Linux/${TARGET_ARCH}"
	@CGO_ENABLED=0 GOOS=linux GOARCH=${TARGET_ARCH} ${GO} build ${GOFLAGS} ${GO_BUILD_FLAGS} -o "${BUILD_DIR}/linux-${TARGET_ARCH}/wavecontrol" ./cmd/server

cross-openbsd:
	@mkdir -p "${BUILD_DIR}/openbsd-${TARGET_ARCH}"
	@echo "==> cross-building OpenBSD/${TARGET_ARCH}"
	@CGO_ENABLED=0 GOOS=openbsd GOARCH=${TARGET_ARCH} ${GO} build ${GOFLAGS} ${GO_BUILD_FLAGS} -o "${BUILD_DIR}/openbsd-${TARGET_ARCH}/wavecontrol" ./cmd/server

cross-windows:
	@mkdir -p "${BUILD_DIR}/windows-${TARGET_ARCH}"
	@echo "==> cross-building Windows/${TARGET_ARCH} executable only"
	@CGO_ENABLED=0 GOOS=windows GOARCH=${TARGET_ARCH} ${GO} build ${GOFLAGS} ${GO_BUILD_FLAGS} -o "${BUILD_DIR}/windows-${TARGET_ARCH}/wavecontrol.exe" ./cmd/server
	@echo "Use windows/build.ps1 or windows/WaveControl.proj for a complete Windows package."

install: env-check install-user install-files install-config install-service
	@echo ""
	@echo "WaveControl installed."
	@echo "Edit ${ENVFILE}; for a new database load ${SHAREDIR}/schema.sql, then run:"
	@echo "  make enable"
	@echo "  make start"

install-user:
	@if [ -n "${DESTDIR}" ]; then \
		echo "==> staged install: not creating ${RUN_USER}"; \
		exit 0; \
	fi; \
	os="${INSTALL_OS}"; [ -n "$$os" ] || os=`uname -s`; \
	if ! grep -q '^${RUN_GROUP}:' /etc/group; then \
		case "$$os" in \
		Linux) groupadd -r "${RUN_GROUP}" ;; \
		OpenBSD) groupadd "${RUN_GROUP}" ;; \
		*) echo "unsupported install host: $$os (supported: Linux, OpenBSD)" >&2; exit 1 ;; \
		esac; \
	fi; \
	if ! id "${RUN_USER}" >/dev/null 2>&1; then \
		case "$$os" in \
		Linux) \
			nologin=/usr/sbin/nologin; [ -x "$$nologin" ] || nologin=/sbin/nologin; \
			useradd -r -g "${RUN_GROUP}" -d "${RUNDIR}" -s "$$nologin" -M "${RUN_USER}" ;; \
		OpenBSD) \
			useradd -g "${RUN_GROUP}" -d "${RUNDIR}" -s /sbin/nologin "${RUN_USER}" ;; \
		*) echo "unsupported install host: $$os (supported: Linux, OpenBSD)" >&2; exit 1 ;; \
		esac; \
	fi; \
	if [ `id -gn "${RUN_USER}"` != "${RUN_GROUP}" ]; then \
		usermod -g "${RUN_GROUP}" "${RUN_USER}"; \
	fi; \
	home=`awk -F: -v user="${RUN_USER}" '$$1 == user { print $$6; exit }' /etc/passwd`; \
	if [ "$$home" != "${RUNDIR}" ]; then \
		echo "${RUN_USER} exists with home $$home; WaveControl requires ${RUNDIR}" >&2; \
		exit 1; \
	fi

install-files:
	@test -x "${BINARY}" || { echo "${BINARY} is missing; run make before make install" >&2; exit 1; }
	@echo "==> installing binary and runtime assets"
	@install -d -m 0755 "${DESTDIR}${BINDIR}"
	@install -m 0755 "${BINARY}" "${DESTDIR}${BINDIR}/wavecontrol"
	@install -d -m 0755 "${DESTDIR}${SHAREDIR}" "${DESTDIR}${DOCDIR}"
	@install -m 0644 schema.sql "${DESTDIR}${SHAREDIR}/schema.sql"
	@rm -rf "${DESTDIR}${SHAREDIR}/migrations"
	@install -d -m 0755 "${DESTDIR}${SHAREDIR}/migrations"
	@cp -R migrations/. "${DESTDIR}${SHAREDIR}/migrations/"
	@find "${DESTDIR}${SHAREDIR}/migrations" -type d -exec chmod 0755 {} \;
	@find "${DESTDIR}${SHAREDIR}/migrations" -type f -exec chmod 0644 {} \;
	@install -m 0644 README.md "${DESTDIR}${DOCDIR}/README.md"
	@rm -rf "${DESTDIR}${DOCDIR}/docs"
	@install -d -m 0755 "${DESTDIR}${DOCDIR}/docs"
	@cp -R docs/. "${DESTDIR}${DOCDIR}/docs/"
	@find "${DESTDIR}${DOCDIR}/docs" -type d -exec chmod 0755 {} \;
	@find "${DESTDIR}${DOCDIR}/docs" -type f -exec chmod 0644 {} \;
	@install -d -m 0750 "${DESTDIR}${RUNDIR}"
	@rm -rf "${DESTDIR}${RUNDIR}/web.new"
	@install -d -m 0755 "${DESTDIR}${RUNDIR}/web.new"
	@cp -R web/. "${DESTDIR}${RUNDIR}/web.new/"
	@find "${DESTDIR}${RUNDIR}/web.new" -type d -exec chmod 0755 {} \;
	@find "${DESTDIR}${RUNDIR}/web.new" -type f -exec chmod 0644 {} \;
	@rm -rf "${DESTDIR}${RUNDIR}/web.old"
	@if [ -d "${DESTDIR}${RUNDIR}/web" ]; then mv "${DESTDIR}${RUNDIR}/web" "${DESTDIR}${RUNDIR}/web.old"; fi
	@if mv "${DESTDIR}${RUNDIR}/web.new" "${DESTDIR}${RUNDIR}/web"; then \
		rm -rf "${DESTDIR}${RUNDIR}/web.old"; \
	else \
		if [ -d "${DESTDIR}${RUNDIR}/web.old" ]; then mv "${DESTDIR}${RUNDIR}/web.old" "${DESTDIR}${RUNDIR}/web"; fi; \
		exit 1; \
	fi
	@install -d -m 0750 "${DESTDIR}${RUNDIR}/firmware" "${DESTDIR}${RUNDIR}/backups"
	@if [ -z "${DESTDIR}" ]; then \
		chown root:"${RUN_GROUP}" "${RUNDIR}"; \
		chown -R root:"${RUN_GROUP}" "${RUNDIR}/web"; \
		chown -R "${RUN_USER}":"${RUN_GROUP}" "${RUNDIR}/firmware" "${RUNDIR}/backups"; \
	fi

install-config:
	@echo "==> installing environment template"
	@install -d -m 0750 "${DESTDIR}${CONFDIR}"
	@install -m 0640 wavecontrol.env.example "${DESTDIR}${CONFDIR}/wavecontrol.env.example"
	@if [ -e "${DESTDIR}${ENVFILE}" ]; then \
		chmod 0600 "${DESTDIR}${ENVFILE}"; \
		echo "preserving existing ${ENVFILE}"; \
	else \
		install -m 0600 wavecontrol.env.example "${DESTDIR}${ENVFILE}"; \
		echo "created ${ENVFILE}; it must be edited before first start"; \
	fi; \
	if [ -z "${DESTDIR}" ]; then chown root "${ENVFILE}"; fi

install-service:
	@os="${INSTALL_OS}"; [ -n "$$os" ] || os=`uname -s`; \
	case "$$os" in \
	Linux) \
		echo "==> installing systemd unit"; \
		install -d -m 0755 "${DESTDIR}${SYSTEMD_UNITDIR}"; \
		install -m 0644 systemd/wavecontrol.service "${DESTDIR}${SYSTEMD_UNITDIR}/wavecontrol.service" ;; \
	OpenBSD) \
		echo "==> installing OpenBSD rc.d script"; \
		install -d -m 0755 "${DESTDIR}${RCDIR}"; \
		install -m 0555 rc.d/wavecontrol "${DESTDIR}${RCDIR}/wavecontrol" ;; \
	*) echo "unsupported install host: $$os (supported: Linux, OpenBSD)" >&2; exit 1 ;; \
	esac

enable:
	@os="${INSTALL_OS}"; [ -n "$$os" ] || os=`uname -s`; \
	case "$$os" in \
	Linux) systemctl daemon-reload && systemctl enable wavecontrol ;; \
	OpenBSD) rcctl enable wavecontrol ;; \
	*) echo "unsupported service host: $$os" >&2; exit 1 ;; \
	esac

start:
	@os="${INSTALL_OS}"; [ -n "$$os" ] || os=`uname -s`; \
	case "$$os" in \
	Linux) systemctl start wavecontrol ;; \
	OpenBSD) rcctl start wavecontrol ;; \
	*) echo "unsupported service host: $$os" >&2; exit 1 ;; \
	esac

restart:
	@os="${INSTALL_OS}"; [ -n "$$os" ] || os=`uname -s`; \
	case "$$os" in \
	Linux) systemctl daemon-reload && systemctl restart wavecontrol ;; \
	OpenBSD) rcctl restart wavecontrol ;; \
	*) echo "unsupported service host: $$os" >&2; exit 1 ;; \
	esac

status:
	@os="${INSTALL_OS}"; [ -n "$$os" ] || os=`uname -s`; \
	case "$$os" in \
	Linux) systemctl status wavecontrol ;; \
	OpenBSD) rcctl check wavecontrol ;; \
	*) echo "unsupported service host: $$os" >&2; exit 1 ;; \
	esac

stop:
	@os="${INSTALL_OS}"; [ -n "$$os" ] || os=`uname -s`; \
	case "$$os" in \
	Linux) systemctl stop wavecontrol ;; \
	OpenBSD) rcctl stop wavecontrol ;; \
	*) echo "unsupported service host: $$os" >&2; exit 1 ;; \
	esac

uninstall:
	@os="${INSTALL_OS}"; [ -n "$$os" ] || os=`uname -s`; \
	if [ -z "${DESTDIR}" ]; then \
		case "$$os" in \
		Linux) systemctl disable --now wavecontrol >/dev/null 2>&1 || true ;; \
		OpenBSD) rcctl stop wavecontrol >/dev/null 2>&1 || true; rcctl disable wavecontrol >/dev/null 2>&1 || true ;; \
		esac; \
	fi; \
	case "$$os" in \
	Linux) \
		rm -f "${DESTDIR}${SYSTEMD_UNITDIR}/wavecontrol.service"; \
		if [ -z "${DESTDIR}" ]; then systemctl daemon-reload >/dev/null 2>&1 || true; fi ;; \
	OpenBSD) rm -f "${DESTDIR}${RCDIR}/wavecontrol" ;; \
	*) echo "unsupported install host: $$os" >&2; exit 1 ;; \
	esac
	@rm -f "${DESTDIR}${BINDIR}/wavecontrol"
	@rm -rf "${DESTDIR}${SHAREDIR}" "${DESTDIR}${DOCDIR}" "${DESTDIR}${RUNDIR}/web"
	@echo "Preserved ${CONFDIR}, firmware, and configuration backups; remove them manually only after backing up secrets and data."

clean:
	@rm -rf "${BUILD_DIR}"

print-config:
	@echo "GO=${GO}"
	@echo "BUILD_DIR=${BUILD_DIR}"
	@echo "BINARY=${BINARY}"
	@echo "DESTDIR=${DESTDIR}"
	@echo "BINDIR=${BINDIR}"
	@echo "SHAREDIR=${SHAREDIR}"
	@echo "CONFDIR=${CONFDIR}"
	@echo "RUNDIR=${RUNDIR}"
	@echo "INSTALL_OS=${INSTALL_OS}"

help:
	@printf '%s\n' \
		'make                         Build WaveControl' \
		'make check                   Run formatting, env-template, test, and vet checks' \
		'make env                     Create ./wavecontrol.env without overwriting it' \
		'make run                     Run a foreground development instance' \
		'make cross-openbsd           Cross-build OpenBSD (TARGET_ARCH=amd64 or arm64)' \
		'make cross-linux             Cross-build Linux (TARGET_ARCH=amd64 or arm64)' \
		'make cross-windows           Cross-build only the Windows executable' \
		'make install                 Install on Linux or OpenBSD; preserves existing env/data' \
		'make enable|start|restart    Manage the installed system service' \
		'make uninstall               Remove program files, preserving config and runtime data' \
		'make clean                   Remove build output'
