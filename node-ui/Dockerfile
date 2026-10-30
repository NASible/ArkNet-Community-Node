FROM node:26-alpine
WORKDIR /app
RUN chown node:node /app
USER node
COPY --chown=node:node package.json package-lock.json ./
RUN npm ci
COPY --chown=node:node index.html tsconfig.json vite.config.ts server.mjs ./
COPY --chown=node:node src ./src
# Start only long enough to read the root-owned TLS key, then server.mjs drops
# permanently to the unprivileged node identity before opening listeners. All
# code imported before that drop must be immutable to the runtime identity.
USER root
RUN chown -R root:root /app && chmod -R go-w /app
ENV ARKNET_UI_HOST=0.0.0.0
EXPOSE 5173 8443
CMD ["node", "server.mjs"]
