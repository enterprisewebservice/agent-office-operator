# File-Based Catalog index image for the Agent Office Operator.
#
# OLM consumes this image via a CatalogSource CR; OperatorHub /
# Ecosystem Software Catalog renders the package as a tile.
#
# We use the upstream operator-framework opm image so pulls don't require
# a registry.redhat.io pull secret in the build pipeline.

FROM quay.io/operator-framework/opm:latest

ENTRYPOINT ["/bin/opm"]
CMD ["serve", "/configs", "--cache-dir=/tmp/cache"]

ADD catalog /configs

# Required label so OLM can find the configs path inside the image.
LABEL operators.operatorframework.io.index.configs.v1=/configs
