# File-Based Catalog index image for the Agent Office Operator.
#
# OLM consumes this image via a CatalogSource CR; OperatorHub /
# Ecosystem Software Catalog renders the package as a tile.

FROM quay.io/operator-framework/opm:latest

ENTRYPOINT ["/bin/opm"]
CMD ["serve", "/configs", "--cache-dir=/tmp/cache"]

ADD catalog /configs

# Pre-warm the cache during build. opm 1.40+ refuses to serve when
# /tmp/cache is empty (integrity check looks for an existing digest), so
# we ask opm itself to populate the cache layout while the image is
# being assembled.
RUN ["/bin/opm", "serve", "/configs", "--cache-dir=/tmp/cache", "--cache-only"]

# Required label so OLM can find the configs path inside the image.
LABEL operators.operatorframework.io.index.configs.v1=/configs
