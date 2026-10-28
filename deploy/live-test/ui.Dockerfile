FROM node:26-alpine AS build
WORKDIR /app
COPY node-ui/package.json node-ui/package-lock.json ./
RUN npm ci
COPY node-ui/index.html node-ui/tsconfig.json node-ui/vite.config.ts ./
COPY node-ui/src ./src
RUN npm run build

FROM nginx:stable-alpine
COPY --from=build /app/dist /usr/share/nginx/html
COPY deploy/live-test/ui-nginx.conf /etc/nginx/conf.d/default.conf
EXPOSE 5173
