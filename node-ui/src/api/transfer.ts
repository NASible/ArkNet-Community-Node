export type TransferState = 'DOWNLOADING' | 'PAUSED' | 'COMPLETED' | 'CANCELLED' | 'VERIFICATION_FAILED';
export interface TransferStatus {
  object_id: string;
  state: TransferState;
  total_bytes: number;
  completed_bytes: number;
  mock: boolean;
  error_code: '' | 'verification_failed';
}
export interface Transfers { enabled: boolean; available: boolean; transfers: TransferStatus[] }
export class TransferError extends Error {
  constructor(public readonly kind: string, message: string) { super(message); this.name = 'TransferError'; }
}
const invalid = () => new TransferError('invalid-payload', 'The Agent returned an invalid transfer response. Check UI and Agent compatibility.');
const record = (value: unknown): value is Record<string, unknown> => typeof value === 'object' && value !== null && !Array.isArray(value);
const bytes = (value: unknown): value is number => typeof value === 'number' && Number.isSafeInteger(value) && value >= 0;
const validId = (value: unknown): value is string => typeof value === 'string' && /^media_[0-9a-f]{64}$/.test(value);
const keys = (value: Record<string, unknown>, allowed: string[]) => Object.keys(value).length === allowed.length && Object.keys(value).every(key => allowed.includes(key));
function transferStatus(value: unknown): TransferStatus {
  if (!record(value) || !keys(value, ['object_id', 'state', 'total_bytes', 'completed_bytes', 'mock', 'error_code']) || !validId(value.object_id) ||
    !['DOWNLOADING', 'PAUSED', 'COMPLETED', 'CANCELLED', 'VERIFICATION_FAILED'].includes(value.state as string) ||
    !bytes(value.total_bytes) || !bytes(value.completed_bytes) || value.completed_bytes > value.total_bytes || typeof value.mock !== 'boolean' ||
    value.error_code !== (value.state === 'VERIFICATION_FAILED' ? 'verification_failed' : '') ||
    (value.state === 'COMPLETED' && value.completed_bytes !== value.total_bytes)) throw invalid();
  return value as unknown as TransferStatus;
}
export type TransferAction = 'start' | 'pause' | 'resume' | 'cancel';
const errors: Record<string, { status: number; message: string }> = {
  catalog_unavailable: { status: 503, message: 'Core catalog unavailable.' },
  media_removed: { status: 409, message: 'This media was removed from the catalog.' },
  media_not_found: { status: 404, message: 'This media could not be found.' },
  local_media_unavailable: { status: 503, message: 'Local media database unavailable.' },
  transfer_unavailable: { status: 503, message: 'Transfer service unavailable.' },
  transfer_conflict: { status: 409, message: 'Transfer state conflict. Refresh before retrying.' },
  transfer_not_found: { status: 404, message: 'Transfer could not be found.' },
  invalid_request: { status: 400, message: 'The transfer request was invalid. Check UI and Agent compatibility.' },
};
function checkResponse(response: Response, data: unknown) {
  if (response.status === 200) return;
  if (record(data) && typeof data.error === 'string' && Object.hasOwn(errors, data.error) && errors[data.error].status === response.status)
    throw new TransferError(data.error, errors[data.error].message);
  throw new TransferError('http-error', `Transfer request failed (HTTP ${response.status}). Check the Node Agent and retry.`);
}
export async function mutateTransfer(id: string, action: TransferAction, signal: AbortSignal): Promise<TransferStatus> {
  if (!validId(id) || !['start', 'pause', 'resume', 'cancel'].includes(action)) throw invalid();
  const response = await fetch(`/api/v1/media/${encodeURIComponent(id)}/transfer/${action}`, {
    method: 'POST', body: '{}', headers: { 'Content-Type': 'application/json' }, signal, cache: 'no-store',
  });
  const data = await json(response);
  checkResponse(response, data);
  const status = transferStatus(data);
  if (status.object_id !== id) throw invalid();
  return status;
}
async function json(response: Response): Promise<unknown> {
  try { return await response.json(); } catch { throw invalid(); }
}
export async function fetchTransfers(signal: AbortSignal): Promise<Transfers> {
  const response = await fetch('/api/v1/transfers', { signal, cache: 'no-store' });
  const data = await json(response);
  checkResponse(response, data);
  if (!record(data) || !keys(data, ['enabled', 'available', 'transfers']) || typeof data.enabled !== 'boolean' || typeof data.available !== 'boolean' || !Array.isArray(data.transfers)) throw invalid();
  const transfers = data.transfers.map(transferStatus);
  if (new Set(transfers.map(item => item.object_id)).size !== transfers.length) throw invalid();
  return { enabled: data.enabled, available: data.available, transfers };
}
