# Zwanga OTP gRPC

Le monolithe NestJS conserve ses routes REST. Lorsque `OTP_GRPC_ENABLED=true`, son module OTP appelle ce service Go via le contrat `proto/otp/v1/otp.proto`. Go génère, envoie et vérifie les codes, puis produit une preuve à usage unique que NestJS consomme avant d'accepter la vérification. Redis est le stockage partagé entre instances Go. Keccel reste le chemin existant lorsque la bascule est désactivée.

## Démarrage local avec le Redis existant

Dans PowerShell, depuis ce dossier :

```powershell
$keyBytes = New-Object byte[] 32
[Security.Cryptography.RandomNumberGenerator]::Create().GetBytes($keyBytes)
$env:OTP_HASH_KEY = [BitConverter]::ToString($keyBytes).Replace('-', '')
$env:OTP_REDIS_ADDRESS = '127.0.0.1:6379'
$env:OTP_REDIS_DB = '15'
$env:OTP_SMS_PROVIDER = 'mock'
go run ./cmd/otp-server
```

Si Redis exige un mot de passe, renseigner `OTP_REDIS_PASSWORD` dans la session. La base 15 évite de mélanger les clés de test avec celles du backend ; vérifier qu'elle convient au serveur Redis avant usage. Le service Go n'efface jamais la base entière. Dans un autre terminal, configurer le backend NestJS avec `OTP_GRPC_ENABLED=true`, `OTP_GRPC_TARGET=localhost:50051`, puis le démarrer selon son README. Aucun appel AWS n'est requis.

Pour lire le dernier code envoyé par le simulateur local, réutiliser les mêmes variables Redis et lancer :

```powershell
go run ./cmd/otp-dev-code +243899123456
```

Cette commande n'est pas incluse dans l'image de production. Le simulateur n'est pas autorisé avec `APP_ENV=production`.

## Parcours REST

Les routes publiques existantes sont conservées :

```text
POST /api/v1/users/phone/send-otp  {"phone":"+243899123456","context":"registration"}
POST /api/v1/users/phone/verify-otp {"phone":"+243899123456","otp":"123456"}
POST /api/v1/auth/pin/reset/request-otp {"phone":"+243899123456"}
POST /api/v1/auth/pin/reset/verify-otp  {"phone":"+243899123456","otp":"123456"}
```

Le client mobile continue d'appeler les mêmes endpoints et de recevoir les mêmes formes de réponse. Les règles d'éligibilité des comptes et de réinitialisation du PIN restent dans NestJS. Un code émis par Keccel avant la bascule ne sera pas validé par Go : activer la bascule après la durée maximale de vie des anciens codes.

## Contrat et sécurité

`SendOtp`, `ResendOtp`, `VerifyOtp`, `CheckOtpStatus` et `ConsumeProof` sont définis dans `otp.v1`. Les types Go sont générés par `protoc`; les types TypeScript du backend sont générés par `proto-loader-gen-types`. La réponse de vérification n'est acceptée par NestJS que si `ConsumeProof` confirme et supprime atomiquement la preuve. Aucun code OTP ou numéro brut n'est inscrit dans les logs.

Le fichier `proto/otp/v1/otp.proto` de ce dépôt est la source du contrat. Avant une modification du protocole, synchroniser sa copie dans `zwanga-backend/proto/otp/v1/otp.proto`, puis régénérer les deux jeux de types :

```powershell
protoc --go_out=. --go_opt=module=otp-grpc --go-grpc_out=. --go-grpc_opt=module=otp-grpc proto/otp/v1/otp.proto
Copy-Item proto/otp/v1/otp.proto ../../zwanga-backend/proto/otp/v1/otp.proto
Push-Location ../../zwanga-backend
npx proto-loader-gen-types --longs=String --enums=String --defaults --oneofs --grpcLib=@grpc/grpc-js -I=proto -O=src/otp/generated proto/otp/v1/otp.proto
Pop-Location
```

Comparer les deux copies du `.proto` dans la revue de code et lancer le test interlangage après chaque évolution du contrat.

Les codes ont six chiffres, cinq minutes de vie par défaut, cinq tentatives, et un délai de renvoi de 60 secondes. Les quotas Redis portent sur le téléphone, et aussi sur l'IP et l'identité si le client gRPC les transmet. La clé `OTP_HASH_KEY` sert aux HMAC du code et des clés Redis ; la conserver identique sur toutes les tâches Go et la protéger comme un secret. Une nouvelle demande avec la même clé d'idempotence ne renvoie jamais un second SMS. La réservation du quota reste consommée si le résultat du fournisseur est ambigu.

En local, le gRPC circule sur `localhost` ou le réseau Docker privé. En production, définir `OTP_SERVICE_TOKEN` (au moins 32 caractères côté Go) dans les secrets des deux services et limiter le trafic réseau. Le chiffrement Service Connect est une étape séparée décrite dans [infra/ecs/README.md](infra/ecs/README.md).

## Mobishastra

Le PDF fourni, version 1.3, décrit l'API d'envoi SMS unitaire `https://mshastra.com/sendurl.aspx` (pages 13-16), pas une API OTP. Le fournisseur Go envoie les paramètres documentés `user`, `pwd`, `senderid`, `mobileno`, `msgtext`, `priority`, `CountryCode` et n'accepte que la réponse `Send Successful`, avec éventuellement un identifiant numérique devant. Les erreurs, y compris celles de transport, sont masquées dans les logs afin de ne pas exposer l'URL contenant les identifiants. Les comptes, Sender IDs approuvés, pays autorisés, crédits et format exact du numéro pour la RDC doivent être confirmés avec Mobishastra avant activation réelle. Le PDF donne surtout des exemples de numéros des Émirats.

Pour utiliser le fournisseur réel : `OTP_SMS_PROVIDER=mobishastra` et configurer `MOBISHASTRA_USER`, `MOBISHASTRA_PASSWORD`, `MOBISHASTRA_SENDER_ID`, `MOBISHASTRA_COUNTRY_CODE`. Le service n'envoie aucun SMS réel en mode `mock`.

## Tests et Docker

```powershell
go test ./...
$env:OTP_INTEGRATION_REDIS_ADDRESS = '127.0.0.1:6379'
$env:OTP_INTEGRATION_REDIS_DB = '15'
go test ./internal/otp -run TestIntegrationRedisGRPC -v
```

Le test d'intégration utilise un numéro aléatoire, un vrai serveur gRPC et Redis ; il teste aussi le redémarrage de Go. Ajouter `OTP_INTEGRATION_REDIS_PASSWORD` si nécessaire. Les autres tests utilisent un Redis éphémère intégré au processus.

Les résultats exécutés et leurs limites figurent dans [docs/VALIDATION_LOCALE.md](docs/VALIDATION_LOCALE.md).

Une fois Docker disponible, depuis ce dossier, avec `OTP_HASH_KEY` et éventuellement `OTP_SERVICE_TOKEN` dans l'environnement :

```powershell
docker compose -f ../../zwanga-backend/docker-compose.yml -f docker-compose.otp.yml up --build api otp-service
docker compose -f ../../zwanga-backend/docker-compose.yml -f docker-compose.otp.yml ps
docker compose -f ../../zwanga-backend/docker-compose.yml -f docker-compose.otp.yml down
```

L'overlay réutilise Postgres, Redis et le backend du Compose existant. Le Go n'expose pas son port sur l'hôte ; NestJS le joint par `otp-service:50051`. Le `healthcheck` appelle `grpc.health.v1.Health`.

## Dépannage

- `OTP storage unavailable` : vérifier l'adresse Redis, le mot de passe et la base sélectionnée. Le healthcheck gRPC doit aussi refléter l'état Redis.
- Réponse HTTP 503 : vérifier `OTP_GRPC_TARGET`, la santé du service Go et la connectivité réseau ; aucun renvoi automatique de SMS n'est effectué.
- Réponse HTTP 429 : attendre le délai de renvoi ou la fin de la fenêtre de quota.
- Code `INVALID` : vérifier que la demande et la validation utilisent le même téléphone E.164 et la même finalité (`phone_verification`, `pin_reset` ou `admin_bootstrap`).
- Échec Mobishastra : vérifier le compte, les crédits, le Sender ID et le pays activé, sans enregistrer l'URL complète ni les identifiants.
