export type LocalState = 'NOT_PRESENT' | 'QUEUED' | 'DOWNLOADING' | 'AVAILABLE' | 'REMOVED_RETAINED' | 'ERROR';
export interface MediaObject {
  id: string;
  title: string;
  size: number;
  state: 'ACTIVE' | 'REMOVED';
  local_state: LocalState;
}
export interface MediaCatalog {
  objects: MediaObject[];
  allocation_bytes: number;
  reserved_bytes: number;
  core_available: boolean;
}
export class MediaError extends Error {
  constructor(public readonly kind: string, message: string) { super(message); this.name = 'MediaError'; }
}
const invalid = () => new MediaError('invalid-payload', 'The Agent returned an invalid media response. Check UI and Agent compatibility.');
const record = (value: unknown): value is Record<string, unknown> => typeof value === 'object' && value !== null && !Array.isArray(value);
const text = (value: unknown): value is string => typeof value === 'string' && value.trim().length > 0;
const bytes = (value: unknown): value is number => typeof value === 'number' && Number.isSafeInteger(value) && value >= 0;
function mediaObject(value: unknown): MediaObject {
  if (!record(value) || !text(value.id) || !/^media_[0-9a-f]{64}$/.test(value.id) || typeof value.title !== 'string' || !bytes(value.size) ||
    !['ACTIVE', 'REMOVED'].includes(value.state as string) ||
    !['NOT_PRESENT', 'QUEUED', 'DOWNLOADING', 'AVAILABLE', 'REMOVED_RETAINED', 'ERROR'].includes(value.local_state as string)) throw invalid();
  return { id: value.id, title: value.title, size: value.size, state: value.state as MediaObject['state'], local_state: value.local_state as LocalState };
}
async function json(response: Response): Promise<unknown> {
  try { return await response.json(); } catch { throw invalid(); }
}
export async function fetchMedia(signal: AbortSignal): Promise<MediaCatalog> {
  const response = await fetch('/api/v1/media', { signal, cache: 'no-store' });
  const data = await json(response);
  checkResponse(response, data);
  if (!record(data) || !Array.isArray(data.objects) || !(data.allocation_bytes === -1 || bytes(data.allocation_bytes)) || !bytes(data.reserved_bytes) || typeof data.core_available !== 'boolean') throw invalid();
  const objects = data.objects.map(mediaObject);
  if (new Set(objects.map(object => object.id)).size !== objects.length) throw invalid();
  return { objects, allocation_bytes: data.allocation_bytes, reserved_bytes: data.reserved_bytes, core_available: data.core_available };
}

const errors: Record<string, { status: number; message: string }> = {
  storage_allocation_exceeded: { status: 409, message: 'Storage allocation exceeded. Review the reserved space and allocation.' },
  media_removed: { status: 409, message: 'This media was removed from the catalog.' },
  media_not_found: { status: 404, message: 'This media could not be found.' },
  media_not_retained: { status: 409, message: 'This media is not a retained removed copy.' },
  media_file_unsafe: { status: 409, message: 'The retained media path is not a regular managed file.' },
  catalog_unavailable: { status: 503, message: 'Core catalog unavailable. Retry when Core is online.' },
  local_media_unavailable: { status: 503, message: 'Local media database unavailable. Check the Node Agent and retry.' },
  invalid_request: { status: 400, message: 'The selection request was invalid. Check UI and Agent compatibility.' },
};
function checkResponse(response: Response, data: unknown) {
  if (response.ok) return;
  if (record(data) && typeof data.error === 'string' && Object.hasOwn(errors, data.error) && errors[data.error].status === response.status)
    throw new MediaError(data.error, errors[data.error].message);
  throw new MediaError('http-error', `Media request failed (HTTP ${response.status}). Check the Node Agent and retry.`);
}
export async function deleteRetainedMedia(id: string, signal: AbortSignal): Promise<void> {
  if (!/^media_[0-9a-f]{64}$/.test(id)) throw invalid();
  const response = await fetch(`/api/v1/media/${encodeURIComponent(id)}/retained`, {
    method: 'DELETE', signal, cache: 'no-store',
  });
  if (response.ok) {
    if (response.status !== 204) throw invalid();
    return;
  }
  const data = await json(response);
  checkResponse(response, data);
}

export async function selectMedia(id: string, signal: AbortSignal): Promise<MediaObject> {
  if (!/^media_[0-9a-f]{64}$/.test(id)) throw invalid();
  const response = await fetch(`/api/v1/media/${encodeURIComponent(id)}/selection`, {
    method: 'POST', body: '{}', headers: { 'Content-Type': 'application/json' }, signal, cache: 'no-store',
  });
  const data = await json(response);
  checkResponse(response, data);
  const object = mediaObject(data);
  if (object.id !== id || object.state !== 'ACTIVE' || !['QUEUED', 'DOWNLOADING', 'AVAILABLE', 'ERROR'].includes(object.local_state)) throw invalid();
  return object;
}
