<?php

namespace App\Door\ZChat\Entity;

use App\Door\ZChat\Entity\Enum\UserStatus;
use App\Door\ZChat\Repository\PresenceRepository;
use App\Entity\User;
use Doctrine\ORM\Mapping as ORM;
use Symfony\Bridge\Doctrine\Types\UuidType;
use Symfony\Component\Uid\Uuid;

#[ORM\Entity(repositoryClass: PresenceRepository::class)]
#[ORM\Table(name: 'zchat_presence')]
#[ORM\Index(columns: ['room_id'], name: 'idx_zchat_presence_room')]
#[ORM\Index(columns: ['last_activity_at'], name: 'idx_zchat_presence_activity')]
class Presence
{
    #[ORM\Id]
    #[ORM\Column(type: UuidType::NAME, unique: true)]
    private Uuid $id;

    #[ORM\OneToOne]
    #[ORM\JoinColumn(nullable: false)]
    private User $user;

    #[ORM\ManyToOne(inversedBy: 'presences')]
    #[ORM\JoinColumn(nullable: true)]
    private ?Room $room = null;

    #[ORM\Column(type: 'string', length: 20, enumType: UserStatus::class)]
    private UserStatus $status = UserStatus::NORMAL;

    #[ORM\Column(length: 35, nullable: true)]
    private ?string $displayAlias = null;

    #[ORM\Column]
    private bool $isInvisible = false;

    #[ORM\Column]
    private \DateTimeImmutable $lastActivityAt;

    #[ORM\Column]
    private \DateTimeImmutable $connectedAt;

    #[ORM\Column(length: 255, nullable: true)]
    private ?string $mercureConnectionId = null;

    public function __construct()
    {
        $this->id = Uuid::v7();
        $this->connectedAt = new \DateTimeImmutable();
        $this->lastActivityAt = new \DateTimeImmutable();
    }

    public function getId(): Uuid
    {
        return $this->id;
    }

    public function getUser(): User
    {
        return $this->user;
    }

    public function setUser(User $user): static
    {
        $this->user = $user;
        return $this;
    }

    public function getRoom(): ?Room
    {
        return $this->room;
    }

    public function setRoom(?Room $room): static
    {
        $this->room = $room;
        return $this;
    }

    public function getStatus(): UserStatus
    {
        return $this->status;
    }

    public function setStatus(UserStatus $status): static
    {
        $this->status = $status;
        return $this;
    }

    public function getDisplayAlias(): ?string
    {
        return $this->displayAlias;
    }

    public function setDisplayAlias(?string $displayAlias): static
    {
        $this->displayAlias = $displayAlias;
        return $this;
    }

    public function getDisplayName(): string
    {
        return $this->displayAlias ?? $this->user->getUsername();
    }

    public function isInvisible(): bool
    {
        return $this->isInvisible;
    }

    public function setIsInvisible(bool $isInvisible): static
    {
        $this->isInvisible = $isInvisible;
        return $this;
    }

    public function getLastActivityAt(): \DateTimeImmutable
    {
        return $this->lastActivityAt;
    }

    public function updateActivity(): static
    {
        $this->lastActivityAt = new \DateTimeImmutable();
        return $this;
    }

    public function getConnectedAt(): \DateTimeImmutable
    {
        return $this->connectedAt;
    }

    public function getMercureConnectionId(): ?string
    {
        return $this->mercureConnectionId;
    }

    public function setMercureConnectionId(?string $mercureConnectionId): static
    {
        $this->mercureConnectionId = $mercureConnectionId;
        return $this;
    }

    public function isAway(): bool
    {
        return $this->status === UserStatus::AWAY;
    }

    public function isBusy(): bool
    {
        return $this->status === UserStatus::BUSY;
    }

    public function isInGame(): bool
    {
        return $this->status === UserStatus::IN_GAME;
    }
}
