"""photo-ml — the archive's optional GPU service.

It holds no state, opens no files under the archive, and talks to no database.
Everything it knows arrives as bytes on a loopback socket and leaves as numbers.
That is not an accident of the implementation: it is what makes the vault
excluded by construction rather than by a WHERE clause somebody can forget to
write, and it is why the systemd unit can put /mnt/photos out of reach entirely.

See ML_IMAGES.md §3 and §6.
"""
