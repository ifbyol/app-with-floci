# ---- build ----
FROM node:18-alpine AS build
WORKDIR /app
COPY package.json package-lock.json* ./
RUN npm install
COPY . .
RUN npm run build

# ---- dev (used by `okteto up`: Vite dev server with hot reload) ----
FROM node:18-alpine AS dev
WORKDIR /app
COPY package.json package-lock.json* ./
RUN npm install
EXPOSE 5173
CMD ["npm", "run", "dev"]

# ---- runtime ----
FROM nginxinc/nginx-unprivileged:1.27-alpine AS runtime
COPY nginx.conf /etc/nginx/conf.d/default.conf
COPY --from=build /app/dist /usr/share/nginx/html
EXPOSE 8080
