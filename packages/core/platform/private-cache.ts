// Blob URLs live outside React Query. Clients register their byte-cache cleanup
// here so revocation and account changes also release those retained bytes.
const cleaners = new Set<() => void>();
export function registerPrivateCacheCleanup(clean: () => void): () => void {
  cleaners.add(clean);
  return () => cleaners.delete(clean);
}
export function clearPrivateCaches(): void {
  for (const clean of cleaners) clean();
}
