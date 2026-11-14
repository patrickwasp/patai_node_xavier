# Open WebUI's slim image as it ships, for the Chat app (docs/apps.md, The Chat app), with what a node does at its start.
# Its health check goes: OCI has no field for one, and the console's ready check does its job.
FROM ghcr.io/open-webui/open-webui:v0.13.3-slim@sha256:8faf917243afee55e042b10ddc08ed6ae6fa9c62522c7e14d81e7cf2922325ae
HEALTHCHECK NONE
COPY patai-start /usr/local/bin/patai-start
# Caught from v0.13.3's own requests on the bench (patai_docs tools/session-2026-11-14/sniff-gateway.py): catch them
# again with each release of Open WebUI, or the warm-up warms nothing.
COPY warm-tools.json /usr/local/share/patai/warm-tools.json
ENTRYPOINT ["/usr/local/bin/patai-start"]
# An entrypoint clears the command the image had.
CMD ["bash", "start.sh"]
LABEL org.opencontainers.image.source=https://github.com/patrickwasp/patai_node_xavier
