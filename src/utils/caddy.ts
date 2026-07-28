import path from 'path';

const ALLOWED_CAMOUFLAGE_DIRS = [
  path.resolve('/var/www'),
  path.resolve('/tmp/camouflage'),
  path.resolve('/opt/route-agent/decoy'),
  path.resolve(process.cwd(), 'tests/temp')
];

/**
 * Валидирует и резолвит путь для фасадной заглушки Caddy, предотвращая Path Traversal.
 */
export function validateSafeCamouflagePath(targetPath: string): string {
  if (!targetPath || typeof targetPath !== 'string') {
    throw new Error('Invalid path provided');
  }

  const resolved = path.resolve(targetPath);

  const isAllowed = ALLOWED_CAMOUFLAGE_DIRS.some((allowedDir) => {
    const relative = path.relative(allowedDir, resolved);
    return !relative.startsWith('..') && !path.isAbsolute(relative);
  });

  if (!isAllowed) {
    throw new Error(`Path Traversal restriction: ${targetPath} is not within allowed directories`);
  }

  return resolved;
}
