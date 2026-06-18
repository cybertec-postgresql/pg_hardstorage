# Created by: Hans-Jürgen Schönig <hs@cybertec.at>
#
# FreeBSD port for pg_hardstorage.
#
# This Makefile lives in-tree as a STARTING POINT for a real
# port submission to the FreeBSD ports tree.  It cannot be
# built on Linux — `bsd.port.mk` requires FreeBSD's bmake and
# the ports infrastructure under /usr/ports/Mk.  The
# run_pkg_build_testing.sh driver validates the file exists
# and the required port variables are set; actual building
# happens on a FreeBSD host (or via Cirrus CI's FreeBSD VM).
#
# To build on a FreeBSD host:
#
#   1. Drop this directory into /usr/ports/databases/pg-hardstorage/.
#   2. Generate distinfo:  cd /usr/ports/databases/pg-hardstorage && make makesum
#   3. Build:               make package

PORTNAME=	pg-hardstorage
DISTVERSION=	0.1.1
CATEGORIES=	databases
MASTER_SITES=	https://github.com/cybertec-postgresql/pg_hardstorage/archive/refs/tags/v${DISTVERSION}.tar.gz?dummy=/

MAINTAINER=	hs@cybertec.at
COMMENT=	PostgreSQL backup, done right -- agent and CLI
WWW=		https://github.com/cybertec-postgresql/pg_hardstorage

LICENSE=	APACHE20
LICENSE_FILE=	${WRKSRC}/LICENSE

USES=		go:modules
GO_MODULE=	github.com/cybertec-postgresql/pg_hardstorage
GO_TARGET=	./cmd/pg_hardstorage
GO_BUILDFLAGS=	-trimpath -buildvcs=false \
		-ldflags="-s -w \
		-X github.com/cybertec-postgresql/pg_hardstorage/internal/version.Version=v${DISTVERSION} \
		-X github.com/cybertec-postgresql/pg_hardstorage/internal/version.Commit=freebsd-port"

PLIST_FILES=	bin/pg_hardstorage \
		man/man1/pg_hardstorage.1.gz \
		share/examples/pg_hardstorage/pg_hardstorage.yaml.example

OPTIONS_DEFINE=	DOCS COMPAT
OPTIONS_DEFAULT=	DOCS COMPAT
COMPAT_DESC=	Build pgBackRest / Barman / WAL-G drop-in shims

post-install:
	${INSTALL_DATA} ${WRKSRC}/deploy/config/pg_hardstorage.yaml.example \
		${STAGEDIR}${PREFIX}/share/examples/pg_hardstorage/pg_hardstorage.yaml.example

post-install-COMPAT-on:
	cd ${WRKSRC} && ${SETENV} ${MAKE_ENV} ${MAKE_CMD} build-compat
	${INSTALL_PROGRAM} ${WRKSRC}/bin/pg-hardstorage-pgbackrest \
		${STAGEDIR}${PREFIX}/bin/pg-hardstorage-pgbackrest
	${INSTALL_PROGRAM} ${WRKSRC}/bin/pg-hardstorage-barman \
		${STAGEDIR}${PREFIX}/bin/pg-hardstorage-barman
	${INSTALL_PROGRAM} ${WRKSRC}/bin/pg-hardstorage-barman-wal-archive \
		${STAGEDIR}${PREFIX}/bin/pg-hardstorage-barman-wal-archive
	${INSTALL_PROGRAM} ${WRKSRC}/bin/pg-hardstorage-walg \
		${STAGEDIR}${PREFIX}/bin/pg-hardstorage-walg

.include <bsd.port.mk>
