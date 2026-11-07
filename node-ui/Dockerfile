# Build stage: development dependencies and sources stay here. The static
# output is platform-independent, so it always builds natively.
FROM --platform=$BUILDPLATFORM node:26-alpine AS build
WORKDIR /app
RUN chown node:node /app
USER node
COPY --chown=node:node package.json package-lock.json ./
RUN npm ci
COPY --chown=node:node index.html tsconfig.json vite.config.ts ./
COPY --chown=node:node src ./src
RUN npm run build

# Runtime: Node itself, the static build and the dependency-free server only.
# No Vite, node_modules, sources, source maps or package managers.
FROM node:26-alpine
RUN rm -rf /usr/local/lib/node_modules /usr/local/bin/npm /usr/local/bin/npx /usr/local/bin/corepack \
        /usr/local/bin/yarn /usr/local/bin/yarnpkg /opt/yarn-*
WORKDIR /app
COPY --from=build /app/dist ./dist
COPY server.mjs start-ui.sh ./
# Start only long enough to read the root-owned TLS key, then server.mjs drops
# permanently to the unprivileged node identity before opening listeners. All
# code imported before that drop must be immutable to the runtime identity.
RUN chown -R root:root /app && chmod -R go-w /app
ENV ARKNET_UI_HOST=0.0.0.0
EXPOSE 5173 8443
CMD ["sh", "./start-ui.sh"]
