FROM node:22-alpine

RUN addgroup -S app && adduser -S app -G app
USER app

WORKDIR /app
COPY --chown=app:app package.json main.js ./

EXPOSE 3000
ENTRYPOINT ["node", "main.js"]
