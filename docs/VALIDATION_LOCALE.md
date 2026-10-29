# Validation locale du 28 octobre 2026

## Exécuté

| Vérification | Résultat |
| --- | --- |
| `go test ./...` avec `OTP_INTEGRATION_REDIS_ADDRESS=127.0.0.1:6379` et DB 15 | Réussi. Tests unitaires avec Redis éphémère et test gRPC avec le Redis actif, y compris redémarrage du serveur, expiration, quotas, concurrence et preuve à usage unique. |
| `npm test -- --runInBand src/otp/otp-grpc.client.spec.ts src/otp/otp-grpc-routing.spec.ts src/otp/otp.service.spec.ts` dans `zwanga-backend` | Réussi : 3 suites, 33 tests. |
| `RUN_OTP_GRPC_INTEGRATION=true npm test -- --runInBand src/otp/otp-grpc.integration.spec.ts` avec Go en écoute sur `127.0.0.1:50051` et Redis DB 15 | Réussi : NestJS a envoyé un OTP, lu le code simulé, vérifié le code et consommé la preuve via le vrai contrat Protobuf. |
| `npm run build` dans `zwanga-backend` | Réussi après alignement de `@nestjs/core`, `@nestjs/common` et `@nestjs/microservices` en 11.2.8. |
| `docker compose -f ../../zwanga-backend/docker-compose.yml -f docker-compose.otp.yml config --quiet` | Réussi : configuration Compose valide. |
| Audit npm après alignement NestJS | Aucune vulnérabilité élevée signalée ; sept alertes faibles restent dans l'arbre de dépendances. |

## Non exécuté

- Construction et démarrage Docker : le client Docker est installé mais le moteur Docker Desktop n'était pas accessible pendant la validation. La demande d'autorisation pour le démarrer en arrière-plan a été refusée. Les health checks et la résolution DNS `otp-service` restent à vérifier en conteneurs.
- Envoi SMS Mobishastra réel : aucun identifiant ou Sender ID approuvé n'a été utilisé. Le simulateur a couvert les parcours locaux ; la réponse réelle et la couverture des numéros RDC doivent être confirmées avec le fournisseur.
- `terraform validate` / `plan` et tout déploiement AWS : le modèle ECS est préparatoire et aucune ressource AWS n'a été créée.
- Parcours REST contre le monolithe complet : les routes existantes ont été conservées et les tests de routage OTP passent, mais le backend complet n'a pas été démarré pour un test HTTP bout en bout.

Le test d'intégration local a utilisé un numéro de test aléatoire et une base Redis distincte. Aucun code OTP, identifiant fournisseur ou mot de passe n'a été ajouté à ce document.
