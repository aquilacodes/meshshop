# Keycloak, pre-built ("optimized") for meshshop so it starts fast and runs with
# a read-only root filesystem. Base image pinned by digest.
ARG KEYCLOAK=quay.io/keycloak/keycloak:26.8.2@sha256:7e66a96e4739239070aeaf72e4c5decf0f7f682cfc725b9f7ea2b336eaba38b3

FROM ${KEYCLOAK} AS build
# Build-time options. dev-file (embedded H2, on a PersistentVolume) keeps the
# playground self-contained; the realm is imported from realm/meshshop-realm.json
# on first start. Use KC_DB=postgres (and a real database) in production.
ENV KC_DB=dev-file \
    KC_HEALTH_ENABLED=true \
    KC_HTTP_RELATIVE_PATH=/auth
RUN /opt/keycloak/bin/kc.sh build

FROM ${KEYCLOAK}
COPY --from=build /opt/keycloak/ /opt/keycloak/
USER 1000
ENTRYPOINT ["/opt/keycloak/bin/kc.sh"]
