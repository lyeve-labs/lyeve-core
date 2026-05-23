package db

// WriteLockName exposes the server-side lock name so a test can ask the
// server whether a session still holds it.
var WriteLockName = writeLockName

// AdminSeatLock is the key the seat guard locks, so a test can hold it.
const AdminSeatLock = adminSeatLock
