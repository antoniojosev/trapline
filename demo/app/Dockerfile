# The demo's buggy service. Nothing here is interesting except that the SDK is
# the official one, pinned to the same version the compatibility matrix tests.
FROM node:24-alpine

WORKDIR /app

# Dependencies first so that rebuilding after the patch — which the demo does,
# and which a viewer is watching — reuses the layer instead of reinstalling.
COPY package.json package-lock.json ./
RUN npm ci --omit=dev --no-fund --no-audit

COPY src ./src
COPY test ./test

ENV NODE_ENV=production
EXPOSE 3000

# Not root, for the same reason the server image is not: a demo is where
# people copy their defaults from.
USER node

CMD ["node", "src/server.js"]
