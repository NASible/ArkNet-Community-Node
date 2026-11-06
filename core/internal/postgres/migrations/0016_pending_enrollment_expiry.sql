-- Pending enrollment expiry is anchored to initial creation, never retries.
-- Keep expired rows from being approved even before opportunistic cleanup runs.
-- Returning NULL makes the existing conditional UPDATE report no decision,
-- preserving the administration repository's enrollment-conflict semantics.
CREATE FUNCTION arknet_prevent_expired_enrollment_approval() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
    BEGIN
        IF OLD.enrollment_status = 'PENDING'
           AND NEW.enrollment_status = 'APPROVED'
           AND OLD.created_at <= clock_timestamp() - interval '7 days' THEN
            RETURN NULL;
        END IF;
        RETURN NEW;
    END
    $$;

CREATE TRIGGER nodes_no_expired_enrollment_approval
    BEFORE UPDATE ON nodes
    FOR EACH ROW EXECUTE FUNCTION arknet_prevent_expired_enrollment_approval();
