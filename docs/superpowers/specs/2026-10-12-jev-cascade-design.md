# Jev como pre-clasificador en cascada — Diseño

- **Fecha:** 2026-10-12
- **Estado:** aprobado en conversación, pendiente de revisión escrita
- **Rama:** `feat/jev-cascade`

## 1. Objetivo

Integrar `typesafe/jev-1.13` (modelo de decisión de TypeSafe servido por OpenRouter) como **primera etapa barata** de la clasificación, respetando su naturaleza: devuelve una opción de una lista cerrada con probabilidades calibradas, sin texto libre. El LLM configurado actualmente (OpenAI / Gemini / DeepSeek) pasa a ser el respaldo que sólo se invoca cuando hace falta.

**Prioridad:** minimizar coste (llamadas al LLM) sin empeorar la calidad actual.

### Criterios de éxito

- Con Jev activado, la mayoría de transacciones con comercio reconocible se clasifican sin llamar al LLM.
- Ningún fallo de Jev provoca un fallo de job que hoy no ocurriría: cualquier error de Jev degrada al comportamiento actual (sólo LLM).
- Con Jev desactivado (por defecto), el comportamiento es idéntico al actual.
- El origen de cada decisión (`jev`, `jev+llm`, `llm`) es visible en la página Jobs.

### Fuera de alcance

- `SuggestTransfer` (sugerencia de cuenta destino para conversión a transferencia) sigue usando el LLM; migrarlo a Jev es una fase posterior.
- Calibración automática de umbrales con datos etiquetados.
- Usar Jev como verificador del LLM.

## 2. Naturaleza de Jev (resumen de la documentación)

- Endpoint: `POST https://openrouter.ai/api/alpha/decisions` (API alpha). **No** funciona con `chat/completions`.
- Petición: `{ model, state, questions }`. `state` es string, objeto o array. `questions` es un objeto indexado por IDs propios; cada pregunta tiene `type` (`choice` | `noul` | `score`), `instructions` y `criteria`. Todas las preguntas se responden en paralelo sobre el mismo `state` y no ven las respuestas de las demás.
- Respuesta `choice`: `{ type, choice, confidence?, probabilities? }`. `probabilities` y `confidence` son opcionales según el esquema.
- Sin razonamiento ni texto libre. Contexto de 32k tokens (state + preguntas). Entrada ~0,042 $/M tokens; salida gratuita. Cada respuesta incluye `usage.cost`.
- Errores: `{ error: { code, message, metadata? } }` con códigos 400, 401, 402, 403, 404, 413, 429, 500, 502, 503, 524, 529.

## 3. Arquitectura

Se elige el **enfoque de decorador**: un `CascadeClassifier` que implementa la interfaz existente `classifier.Classifier` y envuelve al LLM. El pipeline (`internal/pipeline/pipeline.go`) no cambia su flujo; webhook, batch y CLI se benefician automáticamente.

```
pipeline.RunWithOptions
  └─ atajo por historial (sin cambios)
  └─ classifier.Classify  ──►  CascadeClassifier
                                 ├─ JevClient.Decide  (1 petición: category [+ destination])
                                 ├─ decide según umbrales
                                 └─ llm.Classify       (sólo franja media o CREATE)
```

### 3.1 Componentes nuevos (`internal/classifier/`)

| Archivo | Contenido |
|---|---|
| `jev.go` | Cliente HTTP (`net/http`, sin dependencias nuevas). Tipos `decisionRequest` / `decisionResponse`. Interfaz interna `decider { Decide(ctx, decisionRequest) (decisionResponse, error) }`. Error tipado `*JevError{Status int; Message string}`. Timeout 15 s. |
| `cascade.go` | `CascadeClassifier{ jev decider; llm Classifier; th Thresholds }` implementando `Classifier`. Construcción de la petición, traducción de claves sintéticas, lógica de decisión, composición de `Result`. |
| `candidates.go` | `selectCandidates(accounts []AccountCandidate, destination, description string, history []HistoricalEntry, limit int) []AccountCandidate`. |

### 3.2 Cambios en código existente

- `classifier.Result`: nuevo campo `Source string` (`"jev"`, `"jev+llm"`, `"llm"`). Los clasificadores LLM existentes lo dejan vacío; el pipeline trata vacío como `"llm"`.
- `classifier.Request`: sin cambios. Las cuentas del historial se obtienen de `Request.History[].DestinationAccountID`, que ya existe.
- `classifier.BuildParams` / `Build()`: nuevos campos Jev; si `JevEnabled && OpenRouterKey != ""`, devuelve `NewCascade(llm, jevClient, thresholds)`. Si `JevEnabled` sin clave → `slog.Warn` y devuelve sólo el LLM.
- `job.Job`: nuevo campo `Source string \`json:"source,omitempty"\``; `Registry.SetFinished` lo recibe.
- `pipeline`: pasa `result.Source` a `SetFinished`. En el atajo por historial, `Source = "history"`.
- `config`, `api/handler.go`, `public/index.html`, `public/app.js`, `README.md`, `public/docs.html`: ver §6.

## 4. Petición a Jev

Una sola petición por transacción:

```json
{
  "model": "typesafe/jev-1.13",
  "state": {
    "transaction": { "destination": "<dest o descripción si es genérico>", "description": "...", "amount": 42.10 },
    "history": [ { "description": "...", "category": "Groceries" } ],
    "account_holder_context": "<CustomSystemContext, omitido si vacío>"
  },
  "questions": {
    "category": {
      "type": "choice",
      "instructions": "Which of the account holder's spending categories does this bank withdrawal belong to? Use the past classifications for the same payee as strong evidence.",
      "criteria": { "c0": "Groceries — <notas de la categoría>", "c1": "Dining" }
    },
    "destination": {
      "type": "choice",
      "instructions": "Which existing payee (expense) account does this withdrawal go to? Choose \"new\" only if the payee is clearly a specific business that is not in the list.",
      "criteria": { "a0": "Mercadona", "a1": "Amazon", "new": "None of the listed accounts; the payee is a specific business not in the list." }
    }
  }
}
```

- **Claves sintéticas** (`c<i>`, `a<i>`, `new`): la documentación no especifica restricciones de caracteres en las claves y los nombres de categorías/cuentas pueden contener espacios o acentos. Se mantiene un mapa clave → `Category` / `AccountCandidate` para traducir la respuesta.
- `criteria` de categoría: `"<Nombre> — <Notas>"` si hay notas, `"<Nombre>"` si no.
- `history`: `Request.History` (ya recortado por el pipeline a `HistoryContextLimit`). Las entradas sin categoría se omiten.
- `destination` sólo se incluye si `req.DestinationMatching && !req.CategoryOnly` y hay al menos un candidato.
- **Candidatos de destino** (`selectCandidates`, límite 50):
  1. Se descartan nombres genéricos (`isBlankName`).
  2. Cuentas cuyo ID aparece en `History[].DestinationAccountID` entran siempre.
  3. El resto se ordena por similitud con `GroupKey(destination, description)`: puntuación = proporción de tokens (minúsculas, sin puntuación) compartidos, con desempate por subcadena contenida. Se toman hasta completar el límite.

## 5. Lógica de decisión

**Puntuación de una respuesta `choice`:** `p = probabilities[choice]`; si `probabilities` falta, `p = confidence`; si ambos faltan, la respuesta se considera inválida.

Umbrales por defecto: `High = 0.85`, `Low = 0.50`. Bandas: alta `p ≥ High`, media `Low ≤ p < High`, baja `p < Low`.

**Destino** (cuando se preguntó):

- `destOK` = elección es una cuenta existente con `p ≥ High` → `Destination{Name, Action: "MATCH", Confidence: "CLASSIFIED"}`.
- `destNew` = elección es `new` con `p ≥ High`.
- En cualquier otro caso, sin destino.

**Categoría:**

| Banda categoría | Destino | Llamadas LLM | Resultado |
|---|---|---|---|
| alta | `destOK` o sin destino | 0 | `CLASSIFIED`, categoría de Jev, destino MATCH si `destOK`. `Source = "jev"` |
| alta | `destNew` | 1 (`DestinationMatching: true`) | `CLASSIFIED`, categoría **de Jev**; destino = `llmResult.Destination` sólo si su `Action == "CREATE"` y `Confidence == "CLASSIFIED"`, si no, sin destino. `Source = "jev+llm"` |
| media | cualquiera | 1 (petición original completa) | El `Result` del LLM tal cual (outcome, categoría, destino, reason, assumption). Si el LLM no devuelve destino y Jev dio `destOK`, se usa el de Jev. `Source = "jev+llm"` |
| baja | `destOK` o sin destino | 0 | `NEEDS_REVIEW`, sin categoría, destino MATCH si `destOK`. `Source = "jev"` |
| baja | `destNew` | 1 (`DestinationMatching: true`) | `NEEDS_REVIEW`; destino del LLM con las mismas condiciones que en la fila 2. `Source = "jev+llm"` |

- Las peticiones con `req.SystemPromptOverride != ""` van directamente a `llm.Classify` sin pasar por Jev (`Source = "llm"`).
- **`Reason`** cuando decide Jev: `Jev: <Categoría> (0.92) · next: <Segunda> (0.05)`; en NEEDS_REVIEW: `Jev: low confidence — best guess <Categoría> (0.31)`. Si hubo destino de Jev, se añade ` · payee: <Cuenta> (0.97)`. Probabilidades con 2 decimales.
- **`RawPrompt` / `RawResponse`**: JSON (indentado) de la petición y respuesta de Jev. Si además se llamó al LLM, se concatenan con un separador `\n\n--- LLM ---\n\n` el prompt y respuesta del LLM.

## 6. Configuración e interfaz

### 6.1 Campos

| Campo `Config` | Env var | Defecto | `StoredConfig` |
|---|---|---|---|
| `JevEnabled bool` | `JEV_ENABLED` | `false` | `*bool` `jev_enabled` |
| `OpenRouterKey string` | `OPENROUTER_API_KEY` | `""` | `string` `openrouter_api_key` |
| `JevModel string` | `JEV_MODEL` | `typesafe/jev-1.13` | `string` `jev_model` |
| `JevHighThreshold float64` | `JEV_HIGH_THRESHOLD` | `0.85` | `*float64` `jev_high_threshold` |
| `JevLowThreshold float64` | `JEV_LOW_THRESHOLD` | `0.50` | `*float64` `jev_low_threshold` |

- `configResponse` expone `jev_enabled`, `openrouter_key_set` (nunca la clave), `jev_model`, `jev_high_threshold`, `jev_low_threshold`.
- `mergeConfigUpdate`: la clave sólo se sobrescribe si no está vacía (igual que las demás). `putConfig` valida `0 ≤ low ≤ high ≤ 1` sobre el resultado efectivo y responde **400** si no se cumple.
- `IsConfigured()` no cambia: el LLM sigue siendo obligatorio como respaldo.
- `reloadClients()` y `runBatch()` pasan los campos nuevos a `BuildParams`.

### 6.2 UI

- **Settings:** panel *"Jev pre-classifier (OpenRouter)"* bajo *AI Provider* con: interruptor, clave (con pista de "clave guardada"), modelo, umbral alto y bajo (`type=number`, `step=0.05`, `min=0`, `max=1`) y texto de ayuda: *"Jev decides the category with calibrated probabilities; only uncertain cases are sent to the AI provider above."*
- **Jobs:** etiqueta junto al outcome según `source`: `History`, `Jev`, `Jev → LLM`, `LLM`.
- **Docs:** sección en `README.md` (tabla de env vars + explicación de la cascada) y en `public/docs.html`.

## 7. Manejo de errores

| Situación | Comportamiento |
|---|---|
| Error de red, timeout, 429, 5xx, 524, 529 | `slog.Warn`; `llm.Classify(req)`; `Source = "llm"`; se antepone `"Jev unavailable — "` al `Reason`. |
| 401, 402, 403 | Igual, pero `slog.Error` (problema de configuración o crédito). |
| 400, 404, 413 | Igual, con `slog.Error`. |
| Respuesta sin `category`, clave desconocida, sin probabilidad ni confianza, JSON inválido | Tratado como error de Jev → LLM. |
| `destination` ausente o inválida con `category` válida | Se ignora el destino; sin llamada extra al LLM. |
| Falla el LLM en banda media | Se devuelve el error (job fallido, igual que hoy). |
| Falla el LLM en llamada sólo para CREATE | Se conserva el resultado de Jev sin destino; `slog.Warn`. |

## 8. Tests

TDD, sin red real. El repo no tiene tests actualmente; sólo se cubre lo nuevo.

- `internal/classifier/jev_test.go` (`httptest.Server`): forma de la petición (método, ruta, `Authorization: Bearer`, `model`, `state`, `questions`, claves sintéticas, opción `new`); parseo con campos opcionales ausentes; mapeo de cada código de error a `*JevError`; timeout.
- `internal/classifier/cascade_test.go` (dobles de `decider` y `Classifier`): una prueba por fila de la tabla de §5 verificando resultado y **número de llamadas al LLM**; cada caso de §7; bypass por `SystemPromptOverride`; formato de `Reason`, `RawPrompt`, `RawResponse` y `Source`.
- `internal/classifier/candidates_test.go`: ranking por similitud, límite, inclusión forzada de cuentas del historial, descarte de nombres genéricos.
- `internal/config` y `internal/api`: `ApplyStored` / `mergeConfigUpdate` con los campos nuevos; `putConfig` rechaza umbrales inválidos con 400.

**Verificación manual:** `JEV_ENABLED=true ./firefly-ai-categorize --batch` contra una instancia real; revisar en Jobs la proporción de `Jev` frente a `Jev → LLM` y la calidad de las decisiones para ajustar umbrales.
