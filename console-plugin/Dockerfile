# Two-stage build for the Console dynamic plugin.
#
# Stage 1: webpack-build the React bundle (TypeScript + PatternFly).
# Stage 2: serve the resulting static files via the OpenShift nginx
#          unprivileged image — Console fetches the plugin's
#          `plugin-manifest.json` + bundle JS/CSS from this Service.

FROM registry.access.redhat.com/ubi9/nodejs-22:latest AS builder
WORKDIR /opt/app-root/src
COPY --chown=1001:0 package.json tsconfig.json webpack.config.ts console-extensions.json ./
COPY --chown=1001:0 src ./src
RUN npm install --legacy-peer-deps && npm run build

FROM registry.access.redhat.com/ubi9/nginx-124:latest
COPY nginx.conf /etc/nginx/nginx.conf
COPY --from=builder /opt/app-root/src/dist /usr/share/nginx/html
EXPOSE 9001
USER 1001
CMD ["nginx", "-g", "daemon off;"]
