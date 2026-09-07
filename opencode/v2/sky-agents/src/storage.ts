import { randomUUID } from "node:crypto"
import { constants } from "node:fs"
import { lstat, mkdir, open, rename, rm } from "node:fs/promises"
import { dirname, join, parse, resolve } from "node:path"

export async function entry(path: string) {
  try {
    return await lstat(path)
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ENOENT") return undefined
    throw error
  }
}

// Never resolve through a symlink. Reject ancestors writable by other users
// (root-owned sticky temporary directories are safe for private child dirs).
export async function safeDirectory(directory: string, create = false): Promise<boolean> {
  const absolute = resolve(directory)
  let current = parse(absolute).root
  for (const part of absolute.slice(current.length).split("/").filter(Boolean)) {
    current = join(current, part)
    let info = await entry(current)
    if (!info && create) {
      await mkdir(current, { mode: 0o700 }).catch((error: NodeJS.ErrnoException) => {
        if (error.code !== "EEXIST") throw error
      })
      info = await entry(current)
    }
    if (!info) return false
    const stickyRoot = info.uid === 0 && Boolean(info.mode & 0o1000) && current !== absolute
    if (!info.isDirectory() || info.isSymbolicLink() ||
        (info.uid !== 0 && info.uid !== process.getuid?.()) ||
        ((info.mode & 0o022) !== 0 && !stickyRoot)) {
      throw new Error(`Unsafe directory: ${current}`)
    }
  }
  return true
}

export async function readBounded(path: string, maximum: number): Promise<string | undefined> {
  if (!await safeDirectory(dirname(path))) return undefined
  const info = await entry(path)
  if (!info) return undefined
  if (!info.isFile() || info.isSymbolicLink() || info.nlink !== 1 || (info.mode & 0o022) !== 0 ||
      (info.uid !== 0 && info.uid !== process.getuid?.())) throw new Error(`Unsafe file: ${path}`)
  const file = await open(path, constants.O_RDONLY | constants.O_NOFOLLOW | constants.O_NONBLOCK)
  try {
    const opened = await file.stat()
    if (!opened.isFile() || opened.dev !== info.dev || opened.ino !== info.ino || opened.size > maximum) {
      throw new Error(`File has changed or is too large: ${path}`)
    }
    const buffer = Buffer.alloc(maximum + 1)
    let size = 0
    while (size <= maximum) {
      const { bytesRead } = await file.read(buffer, size, buffer.length - size, null)
      if (!bytesRead) break
      size += bytesRead
    }
    if (size > maximum) throw new Error(`File is too large: ${path}`)
    return buffer.subarray(0, size).toString("utf8")
  } finally {
    await file.close()
  }
}

export async function locked<T>(directory: string, action: () => Promise<T>): Promise<T> {
  await safeDirectory(directory, true)
  const path = join(directory, ".sky-agents.lock")
  const file = await open(path, "wx", 0o600).catch((error: NodeJS.ErrnoException) => {
    if (error.code === "EEXIST") throw new Error("Another Sky Agents operation is in progress; try again")
    throw error
  })
  try {
    return await action()
  } finally {
    await file.close()
    await rm(path, { force: true })
  }
}

export async function temporaryFile(path: string, text: string, mode = 0o600) {
  await safeDirectory(dirname(path))
  const temporary = `${path}.sky-agents-${randomUUID()}.tmp`
  const file = await open(temporary, "wx", mode)
  try {
    await file.writeFile(text, "utf8")
    await file.sync()
  } catch (error) {
    await rm(temporary, { force: true })
    throw error
  } finally {
    await file.close()
  }
  return temporary
}

export async function replaceFile(path: string, text: string) {
  // Validate the existing destination too: never follow a backup symlink.
  await readBounded(path, 2 * 1024 * 1024)
  const temporary = await temporaryFile(path, text)
  try {
    await safeDirectory(dirname(path))
    await rename(temporary, path)
  } finally {
    await rm(temporary, { force: true })
  }
}
